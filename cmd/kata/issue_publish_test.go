package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func publishCLIHTTP(t *testing.T, supported bool, handler http.HandlerFunc) (*testenv.Env, string) {
	t.Helper()
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://daemon.example/spoke-project.git")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"issue_publish": supported}))
			return
		}
		if strings.Contains(r.URL.Path, "/issue-sync/") {
			handler(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return &testenv.Env{URL: s.URL}, dir
}
func TestIssuePublishCLI(t *testing.T) {
	for _, p := range []string{"github", "notion", "linear", "plane", "ticktick", "todoist", "twenty"} {
		modes := []string{"human"}
		if p == "notion" {
			modes = append(modes, "json", "agent")
		}
		for _, mode := range modes {
			t.Run(p+"/"+mode, func(t *testing.T) {
				calls := 0
				env, dir := publishCLIHTTP(t, true, func(w http.ResponseWriter, r *http.Request) {
					calls++
					require.Equal(t, http.MethodPost, r.Method)
					require.True(t, strings.HasSuffix(r.URL.Path, "/issue-sync/"+p+"/publish"))
					var b map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
					require.Equal(t, "abc4", b["issue_ref"])
					require.NotEmpty(t, b["actor"])
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"provider": p, "issue_uid": "example-uid", "issue_ref": "abc4", "source_key": p + ":source", "external_id": "native:42", "result": "created", "url": "https://tracker.example/task/42"}))
				})
				args := []string{"sync", p, "publish", "abc4"}
				if mode != "human" {
					args = append(args, "--"+mode)
				}
				out := runCLI(t, env, dir, args...)
				switch mode {
				case "json":
					var result map[string]any
					require.NoError(t, json.Unmarshal([]byte(out), &result))
					require.Equal(t, map[string]any{
						"kata_api_version": float64(1),
						"provider":         p, "issue_uid": "example-uid", "issue_ref": "abc4",
						"source_key": p + ":source", "external_id": "native:42",
						"result": "created", "url": "https://tracker.example/task/42",
					}, result)
				case "agent":
					fields := map[string]string{}
					for field := range strings.FieldsSeq(out) {
						name, value, ok := strings.Cut(field, "=")
						require.True(t, ok, "agent output must contain key-value fields: %q", out)
						fields[name] = value
					}
					require.Equal(t, map[string]string{
						"provider": p, "issue_ref": "abc4", "external_id": "native:42",
						"result": "created", "url": "https://tracker.example/task/42",
					}, fields)
				default:
					require.Contains(t, out, "native:42")
					require.Contains(t, out, "https://tracker.example/task/42")
				}
				require.Equal(t, 1, calls)
				args[3] = "spoke-project#abc4"
				runCLI(t, env, dir, args...)
				require.Equal(t, 2, calls)
			})
		}
	}
}
func TestIssuePublishCLIAbsentCapability(t *testing.T) {
	env, dir := publishCLIHTTP(t, false, func(http.ResponseWriter, *http.Request) { t.Error("unsupported publish reached daemon") })
	_, _, err := runCLIWithErr(t, env, dir, "sync", "notion", "publish", "abc4")
	require.Error(t, err)
	require.Contains(t, err.Error(), "upgrade")
}

func TestIssuePublishDaemonStartupAllProviders(t *testing.T) {
	upstream := testenv.InstallPublicationUpstream(t)
	_, record := readRuntimeRecordFromStartedDaemon(t, "127.0.0.1:0")
	env := &testenv.Env{URL: "http://" + record.Address}
	store := openKataTestDB(t, os.Getenv("KATA_DB"))
	defer func() { _ = store.Close() }()
	dir := initBoundWorkspace(t, env.URL, "https://daemon.example/spoke-project.git")
	p, err := store.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, err)
	for _, provider := range []string{"github", "notion", "linear", "plane", "ticktick", "todoist", "twenty"} {
		t.Run(provider, func(t *testing.T) {
			// Each provider has its own project because a project binds one source.
			project := p
			if provider != "github" {
				var e error
				project, e = store.CreateProject(t.Context(), "example-"+provider)
				require.NoError(t, e)
			}
			i, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Native issue", Author: "worker"})
			require.NoError(t, err)
			b, err := store.UpsertIssueSyncBinding(t.Context(), upstream.Binding(t, project.ID, provider))
			require.NoError(t, err)
			out := runCLI(t, env, dir, "sync", provider, "publish", project.Name+"#"+i.ShortID, "--json")
			require.Contains(t, out, "created")
			require.Equal(t, 1, upstream.Creates(provider))
			var result struct {
				ExternalID string `json:"external_id"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			mapping, err := store.ImportMappingBySource(t.Context(), project.ID, b.SourceKey, "issue", result.ExternalID)
			require.NoError(t, err)
			require.Equal(t, &i.ID, mapping.IssueID)
			require.Contains(t, out, i.UID)
		})
	}
}
func TestIssuePublishCLIStatusRecoveryIdentity(t *testing.T) {
	for _, provider := range []string{"notion", "github"} {
		for _, mode := range []string{"human", "agent"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				env, dir := publishCLIHTTP(t, true, func(w http.ResponseWriter, _ *http.Request) {
					response := cliNotionResponse()
					if provider == "github" {
						response["binding"] = map[string]any{"provider": "github", "source_key": "github:github.com:12345", "display_name": "example-owner/example-repo", "enabled": true, "interval_seconds": 300, "config": map[string]any{"host": "github.com", "owner": "example-owner", "repo": "example-repo"}}
						response["status"].(map[string]any)["provider"] = provider
					}
					response["status"].(map[string]any)["pending_publication"] = map[string]any{"attempt_uid": "attempt-example", "provider": provider, "issue_uid": "issue-example", "phase": "dispatching"}
					require.NoError(t, json.NewEncoder(w).Encode(response))
				})
				args := []string{"sync", provider, "status"}
				if mode == "agent" {
					args = append(args, "--agent")
				}
				out := runCLI(t, env, dir, args...)
				require.Contains(t, out, "attempt-example")
				require.Contains(t, out, "issue-example")
				require.Contains(t, out, "dispatching")
			})
		}
	}
}

func TestIssuePublishCLIRecoveryValidation(t *testing.T) {
	env, dir := publishCLIHTTP(t, true, func(http.ResponseWriter, *http.Request) { t.Error("invalid recovery reached daemon") })
	for _, extra := range [][]string{{"--adopt", "7", "--resolve-not-created"}, {"--resolve-not-created"}, {"--attempt", "attempt-example"}, {"--message", "inspection"}} {
		args := append([]string{"sync", "github", "publish", "abc4"}, extra...)
		_, _, err := runCLIWithErr(t, env, dir, args...)
		require.Error(t, err)
	}
}
