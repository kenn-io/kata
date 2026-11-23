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
	require.LessOrEqual(t, len(agentguide.MCPContract), 2100)
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

// Product contract: every MCP guide and the comment tools say
// comments carry findings, never progress.
func TestNativeGuidesSayCommentsCarryFindings(t *testing.T) {
	session := connectTestServer(t)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	descriptions := map[string]string{}
	for _, tool := range listed.Tools {
		descriptions[tool.Name] = tool.Description
	}
	require.NotRegexp(t, `\bprogress\b`, session.InitializeResult().Instructions)
	require.Contains(t, descriptions["kata.comment"], "finding")
	require.Contains(t, descriptions["kata.edit_comment"], "correct")
	require.Contains(t, descriptions["kata.close"], "not merged")
	for _, name := range []string{"kata.comment", "kata.edit_comment", "kata.close"} {
		require.NotRegexp(t, `\bprogress\b`, descriptions[name], name)
	}
	for _, guide := range skillGuides {
		md, ok := SkillMarkdown(guide.name)
		require.True(t, ok, guide.name)
		require.NotRegexp(t, `\bprogress\b`, md, guide.name)
		for _, text := range []string{"update work.attention and work.attention_msg", "kata.edit_comment", "not merged", "needs-review"} {
			require.Contains(t, md, text, guide.name)
		}
		if guide.name == "quickstart" {
			unfinished := regexp.MustCompile(`(?s)If unfinished,.*?\n\n`).FindString(md + "\n\n")
			require.Regexp(t, `\bwork\.attention\b`, unfinished)
			require.Contains(t, unfinished, "work.attention_msg")
		}
	}
}

// Product contract: MCP guidance tells agents to claim with
// ttl_seconds and renew by claiming again, without if_unowned.
func TestNativeGuidesSayClaimWithTTL(t *testing.T) {
	session := connectTestServer(t)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	texts := map[string]string{"instructions": session.InitializeResult().Instructions}
	for _, tool := range listed.Tools {
		if tool.Name == "kata.claim" {
			texts["kata.claim"] = tool.Description
		}
	}
	for _, name := range []string{"workflow", "quickstart", "claiming"} {
		md, ok := SkillMarkdown(name)
		require.True(t, ok, name)
		texts[name] = md
	}
	for name, text := range texts {
		require.Contains(t, text, "ttl_seconds", name)
		require.Contains(t, text, "without if_unowned", name)
	}
	for _, name := range []string{"quickstart", "claiming"} {
		for _, example := range nativeExamples(t, name) {
			if example.Tool == "kata.claim" {
				require.Contains(t, example.Arguments, "ttl_seconds", name)
			}
		}
	}
}
