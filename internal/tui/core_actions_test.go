package tui

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	for _, scenario := range []string{"KATA_TELEMETRY_ENABLED", "dev"} {
		t.Run(scenario, func(t *testing.T) {
			saved := version.Version
			t.Cleanup(func() { version.Version = saved })
			if scenario == "dev" {
				version.Version = "dev"
			} else {
				t.Setenv(scenario, "0")
			}
			var surfaces []string
			var posts int
			hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/api/v1/ui/telemetry" {
					posts++
				} else {
					surfaces = append(surfaces, req.Header.Get("X-Kata-Surface"))
				}
				return jsonResponse(t, map[string]any{"changed": true}), nil
			})}
			c := NewClient("http://127.0.0.1:7777", hc)
			require.NoError(t, c.ReportAppOpened(t.Context()))
			_, err := c.CreateIssue(t.Context(), 7, CreateIssueBody{Title: "Example task", Actor: "user-a"})
			require.NoError(t, err)
			require.Zero(t, posts)
			assert.Equal(t, []string{""}, surfaces)
		})
	}
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
