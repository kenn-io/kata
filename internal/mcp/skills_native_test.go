package mcpserver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentguide"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
)

// Contract: setup is an initial, read-only tool returning exact caller-owned
// edit content; it cannot accept client paths or claim to have installed files.
func TestNativeSetupBoundary(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "AGENTS.md")
	original := []byte("# Local rules\nKeep these exactly.\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	session := connectRawTestServerWithOptions(t, Options{Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test"})
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Contains(t, toolNames(listed.Tools), "kata.skills")
	out := callWorkflowTool(t, session, "kata.skills", map[string]any{"name": "setup"})
	want, err := agentguide.ManagedBlock("mcp")
	require.NoError(t, err)
	require.Equal(t, want, out["managed_block"])
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.skills", Arguments: map[string]any{"name": "setup"}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	text := result.Content[0].(*sdkmcp.TextContent).Text
	require.Contains(t, text, want)
	for _, field := range []string{"path", "content", "workspace", "agent_mode"} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.skills", Arguments: map[string]any{"name": "setup", field: path}})
		require.NoError(t, err)
		require.True(t, result.IsError, field)
	}
	got, err := os.ReadFile(path) //nolint:gosec // test fixture under t.TempDir
	require.NoError(t, err)
	require.Equal(t, original, got)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

type nativeExample struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

func nativeExamples(t *testing.T, name string) []nativeExample {
	t.Helper()
	md, ok := SkillMarkdown(name)
	require.True(t, ok, name)
	var examples []nativeExample
	for _, match := range regexp.MustCompile("(?s)```json\\n(.*?)\\n```").FindAllStringSubmatch(md, -1) {
		var example nativeExample
		require.NoError(t, json.Unmarshal([]byte(match[1]), &example), name)
		require.NotEmpty(t, example.Tool, name)
		require.NotNil(t, example.Arguments, name)
		examples = append(examples, example)
	}
	return examples
}

// Validate the published JSON against the actual loaded schemas, rather than
// grepping implementation text for the same hand-written argument names.
func TestNativeGuideExamplesMatchCatalog(t *testing.T) {
	session := connectTestServer(t)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	byName := map[string]*sdkmcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"quickstart", "issues", "claiming", "closing", "teammates", "scheduling", "events"} {
		t.Run(name, func(t *testing.T) {
			examples := nativeExamples(t, name)
			require.NotEmpty(t, examples)
			for _, example := range examples {
				tool := byName[example.Tool]
				require.NotNil(t, tool, example.Tool)
				var schema jsonschema.Schema
				require.NoError(t, json.Unmarshal(mustJSON(t, tool.InputSchema), &schema))
				resolved, err := schema.Resolve(nil)
				require.NoError(t, err)
				require.NoError(t, resolved.Validate(example.Arguments), "%s: %s", example.Tool, mustJSON(t, example.Arguments))
			}
		})
	}
	md, _ := SkillMarkdown("workflow")
	require.Contains(t, md, agentguide.MCPContract)
	require.LessOrEqual(t, len(agentguide.MCPContract), 2000)
}

// An MCP-only client follows the published quickstart using fixture state.
// It has no subprocess runner, so every step goes through MCP tools.
func TestNativeQuickstartNoShell(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	client, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	// Multi-project scope requires an explicit create project and qualified refs.
	session := connectRawTestServerWithOptions(t, Options{Client: client, Scope: NewAllScope(), Actor: "example-agent", Version: "test"})
	callWorkflowTool(t, session, "kata.skills", map[string]any{"name": "workflow"})
	ref := ""
	examples := nativeExamples(t, "quickstart")
	require.NotEmpty(t, examples)
	var called []string
	for _, example := range examples {
		args := mustJSON(t, example.Arguments)
		args = bytes.ReplaceAll(args, []byte("spoke-project#abc4"), []byte(ref))
		var input map[string]any
		require.NoError(t, json.Unmarshal(args, &input))
		out := callWorkflowTool(t, session, example.Tool, input)
		called = append(called, example.Tool)
		if strings.HasPrefix(example.Tool, "kata.load_") {
			_, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
		}
		if example.Tool == "kata.create" {
			issue := out["issue"].(map[string]any)
			ref = project.Name + "#" + issue["ref"].(string)
			// A transport retry reuses the same operation key and issue.
			again := callWorkflowTool(t, session, example.Tool, input)
			require.Equal(t, issue["uid"], again["issue"].(map[string]any)["uid"])
		}
		if example.Tool == "kata.comment" {
			again := callWorkflowTool(t, session, example.Tool, input)
			require.Equal(t, out["comment"].(map[string]any)["uid"], again["comment"].(map[string]any)["uid"])
		}
	}
	for _, name := range []string{"kata.search", "kata.create", "kata.claim", "kata.set_metadata", "kata.comment", "kata.close"} {
		require.Contains(t, called, name)
	}
	final := callWorkflowTool(t, session, "kata.show", map[string]any{"ref": ref})["issue"].(map[string]any)
	require.Equal(t, "closed", final["status"])
	require.Equal(t, "example-agent", final["owner"])
}
