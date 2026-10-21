package tui

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/version"
)

func TestCoreActionSurfaceHeaders(t *testing.T) {
	var surfaces []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		surfaces = append(surfaces, r.Header.Get("X-Kata-Surface"))
		respondJSON(t, w, map[string]any{"changed": true})
	})
	_, err := c.CreateIssue(t.Context(), 7, CreateIssueBody{Title: "Example task", Actor: "user-a"})
	require.NoError(t, err)
	_, err = c.Close(t.Context(), 7, "abcd", "user-a")
	require.NoError(t, err)
	_, err = c.CloseWithEvidence(t.Context(), 7, "abcd", CloseInput{Actor: "user-a", Reason: "done"})
	require.NoError(t, err)
	_, err = c.Reopen(t.Context(), 7, "abcd", "user-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"tui", "tui", "tui", ""}, surfaces)
}

func TestUndoCloseSuppressesSurface(t *testing.T) {
	var surfaces []string
	hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/instance":
			return jsonResponse(t, map[string]any{"instance_uid": "instance-1"}), nil
		case "/api/v1/projects/7/issues/issue-1":
			return jsonResponse(t, map[string]any{"issue": map[string]any{
				"uid": "issue-1", "project_id": 7, "status": "open", "revision": 5,
			}}), nil
		case "/api/v1/projects/7/issues/issue-1/actions/close":
			surfaces = append(surfaces, req.Header.Get("X-Kata-Surface"))
			return jsonResponse(t, map[string]any{
				"issue":   map[string]any{"uid": "issue-1", "project_id": 7, "status": "closed", "revision": 6},
				"changed": true,
			}), nil
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})}
	client := NewClient("http://127.0.0.1:7777", hc)
	undo := newUndoClient(client)
	done := "done"
	entry := undoEntry{
		kind: "reopen", uid: "issue-1", projectID: 7, instanceUID: "instance-1", revision: 5,
		actor: "user-a", before: Issue{Status: "closed", ClosedReason: &done, Revision: 4},
		after: Issue{Status: "open", Revision: 5},
	}
	outcome := undo.undo(t.Context(), entry, false, nil)
	require.NoError(t, outcome.err)
	require.True(t, outcome.changed)
	outcome.attempt.complete()

	assert.Equal(t, []string{""}, surfaces)

	outcome = undo.undo(t.Context(), entry, true, &CloseInput{
		Actor: "user-a", Reason: "done", Message: "Completed the requested work and checked the result.",
	})
	require.NoError(t, outcome.err)
	require.True(t, outcome.changed)
	outcome.attempt.complete()

	assert.Equal(t, []string{"", ""}, surfaces)
}

func TestTUIClientTelemetryAdmission(t *testing.T) {
	scenario := os.Getenv("KATA_TUI_ADMISSION_SCENARIO")
	if scenario == "" {
		for _, name := range []string{"release", "CI", "KATA_TELEMETRY_ENABLED", "dev"} {
			t.Run(name, func(t *testing.T) {
				command := exec.Command(os.Args[0], "-test.run=^TestTUIClientTelemetryAdmission$", "-test.timeout=30s") //nolint:gosec // Run this exact client test in a fresh process.
				command.Env = append(os.Environ(), "KATA_TUI_ADMISSION_SCENARIO="+name)
				output, err := command.CombinedOutput()
				require.NoError(t, err, string(output))
			})
		}
		return
	}
	saved := version.Version
	t.Cleanup(func() { version.Version = saved })
	switch scenario {
	case "CI":
		t.Setenv("CI", "")
	case "KATA_TELEMETRY_ENABLED":
		t.Setenv(scenario, "0")
	case "dev":
		version.Version = "dev"
	}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = &recordingTelemetry{} })
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	forward := &httputil.ReverseProxy{Rewrite: func(request *httputil.ProxyRequest) { request.SetURL(target) }}
	var mu sync.Mutex
	var surfaces []string
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path == "/api/v1/ui/telemetry" {
			posts++
		}
		if r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/issues") || strings.HasSuffix(r.URL.Path, "/actions/close")) {
			surfaces = append(surfaces, r.Header.Get("X-Kata-Surface"))
		}
		mu.Unlock()
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	c := NewClient(server.URL, server.Client())
	require.NoError(t, c.ReportAppOpened(t.Context()))
	require.NoError(t, c.ReportScreenViewed(t.Context(), "issues"))
	created, err := c.CreateIssue(t.Context(), project.ID, CreateIssueBody{Title: "Example task", Actor: "user-a"})
	require.NoError(t, err)
	require.NotNil(t, created.Issue)
	closed, err := c.Close(t.Context(), project.ID, created.Issue.ShortID, "user-a")
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Issue.Status)
	issues, err := c.ListIssues(t.Context(), project.ID, ListFilter{})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	mu.Lock()
	defer mu.Unlock()
	if scenario == "release" {
		require.Positive(t, posts)
		assert.Equal(t, []string{"tui", "tui"}, surfaces)
	} else {
		require.Zero(t, posts)
		assert.Equal(t, []string{"", ""}, surfaces)
	}
}
