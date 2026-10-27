package mcpserver

import (
	"encoding/json"
	"maps"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestSkillExamplesSatisfyPublishedSchemas(t *testing.T) {
	session := connectTestServer(t)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	schemas := map[string]*jsonschema.Resolved{}
	for _, tool := range listed.Tools {
		var schema jsonschema.Schema
		require.NoError(t, json.Unmarshal(mustJSON(t, tool.InputSchema), &schema))
		resolved, err := schema.Resolve(nil)
		require.NoError(t, err, tool.Name)
		schemas[tool.Name] = resolved
	}
	for _, example := range skillExamples {
		require.Contains(t, schemas, example.Tool)
		for _, form := range []map[string]any{example.Bound, example.Multi} {
			if form != nil {
				require.NoError(t, schemas[example.Tool].Validate(form), "%s: %s", example.Step, mustJSON(t, form))
			}
		}
	}
	require.Error(t, schemas["kata.claim"].Validate(map[string]any{"ref": "abc4", "project": "spoke-project"}))
}

func TestWorkflowGuideRendersExamples(t *testing.T) {
	md, ok := SkillMarkdown("workflow")
	require.True(t, ok)
	require.Contains(t, md, "## MCP calls for the required steps")
	for _, example := range skillExamples {
		for _, form := range []map[string]any{example.Bound, example.Multi} {
			if form == nil {
				continue
			}
			args, err := json.Marshal(form)
			require.NoError(t, err)
			require.Contains(t, md, "`"+example.Tool+" "+string(args)+"`", example.Step)
		}
	}
}

func TestSkillExamplesRunAgainstDaemon(t *testing.T) {
	for _, mode := range []string{"bound", "allowlist"} {
		t.Run(mode, func(t *testing.T) {
			session, store := newCoordinationSession(t)
			if mode == "allowlist" {
				projects, err := store.ListProjects(t.Context())
				require.NoError(t, err)
				var identities []ProjectIdentity
				for _, project := range projects {
					identities = append(identities, ProjectIdentity{ID: project.ID, UID: project.UID, Name: project.Name})
				}
				scope, err := NewAllowlistScope(identities)
				require.NoError(t, err)
				server := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				httpServer := httptest.NewServer(server.Handler())
				t.Cleanup(httpServer.Close)
				client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
				require.NoError(t, err)
				session = connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "example-agent", Version: "test-version"})
			}
			ref := ""
			for _, example := range skillExamples {
				form := example.Bound
				if mode == "allowlist" && example.Multi != nil {
					form = example.Multi
				}
				args := maps.Clone(form)
				if _, ok := args["ref"]; ok {
					require.NotEmpty(t, ref, example.Step)
					createdID := ref
					if _, id, qualified := strings.Cut(ref, "#"); qualified {
						createdID = id
					}
					// Preserve the published qualifier so a missing or wrong project fails.
					published := args["ref"].(string)
					if project, id, qualified := strings.Cut(published, "#"); qualified {
						args["ref"] = project + "#" + strings.Replace(id, "abc4", createdID, 1)
					} else {
						args["ref"] = strings.Replace(published, "abc4", createdID, 1)
					}
				}
				out := callAdministrationTool(t, session, example.Tool, args)
				if example.Tool == "kata.create" {
					ref = out["issue"].(map[string]any)["ref"].(string)
					if mode == "allowlist" && !strings.Contains(ref, "#") {
						ref = "spoke-project#" + ref
					}
				}
				if example.Tool == "kata.claim" {
					require.Equal(t, "example-agent", out["issue"].(map[string]any)["owner"])
				}
				if example.Tool == "kata.show" {
					issue := out["issue"].(map[string]any)
					require.Equal(t, "open", issue["status"])
					require.Equal(t, "needs-human", issue["metadata"].(map[string]any)["work.attention"])
				}
			}
			shown := callAdministrationTool(t, session, "kata.show", map[string]any{"ref": ref})
			require.Equal(t, "closed", shown["issue"].(map[string]any)["status"])
		})
	}
}

func TestAttentionFallbackIsShowNotInbox(t *testing.T) {
	session, _ := newCoordinationSession(t)
	create := func(title, key string) string {
		out := callAdministrationTool(t, session, "kata.create", map[string]any{"title": title, "idempotency_key": key})
		return out["issue"].(map[string]any)["ref"].(string)
	}
	a := create("Choose expected retry behavior", "attention-example")
	b := create("Finish callback cleanup", "closure-example")
	callAdministrationTool(t, session, "kata.set_metadata", map[string]any{"ref": a, "patch": map[string]any{"work.attention": "needs-human"}})
	shown := callAdministrationTool(t, session, "kata.show", map[string]any{"ref": a})["issue"].(map[string]any)
	require.Equal(t, "open", shown["status"])
	require.Equal(t, "needs-human", shown["metadata"].(map[string]any)["work.attention"])
	inbox := callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "coordinator"})
	require.Empty(t, inbox["requests"])
	callAdministrationTool(t, session, "kata.close", map[string]any{
		"ref": b, "reason": "done", "message": "Finished callback cleanup and checked the retry regression.",
		"evidence": []any{map[string]any{"type": "commit", "sha": "0123456789abcdef0123456789abcdef01234567"}},
	})
	shown = callAdministrationTool(t, session, "kata.show", map[string]any{"ref": b})["issue"].(map[string]any)
	require.Equal(t, "closed", shown["status"])
	inbox = callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "coordinator"})
	require.Empty(t, inbox["requests"])
}

func TestSkillPointersResolve(t *testing.T) {
	session := connectTestServer(t)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	re := regexp.MustCompile(`kata\.skills(?: with)? \{"name":"([a-z-]+)"\}`)
	required := map[string]bool{}
	for _, name := range []string{"kata.search", "kata.create", "kata.claim", "kata.next", "kata.close", "kata.set_metadata", "kata.inbox", "kata.wait", "kata.set_schedule", "kata.set_deadline", "kata.events"} {
		required[name] = true
	}
	check := func(where, text string) int {
		matches := re.FindAllStringSubmatch(text, -1)
		for _, match := range matches {
			require.Contains(t, SkillGuides(), match[1], where)
		}
		return len(matches)
	}
	for _, tool := range listed.Tools {
		count := check(tool.Name, tool.Description)
		if required[tool.Name] {
			require.Positive(t, count, tool.Name)
			delete(required, tool.Name)
		}
	}
	require.Empty(t, required, "all pointer-bearing tools must be published")
	require.Positive(t, check("instructions", session.InitializeResult().Instructions))
}
