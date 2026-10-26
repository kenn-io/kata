package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func TestTeammateResolutionFrozen(t *testing.T) {
	t.Setenv("KATA_TEAMMATE", "worker-1")
	cmd := newRootCmd()
	cmd.SetContext(context.Background())
	first, err := resolveTeammate(cmd)
	require.NoError(t, err)
	t.Setenv("KATA_TEAMMATE", "worker-2")
	second, err := resolveTeammate(cmd)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestTeammateCLIActionHistory(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "Attributed work")
	t.Setenv("KATA_TEAMMATE", "worker-1")
	runCLI(t, env, dir, "edit", ref, "--title", "Revised")
	events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: pid, Limit: 100})
	require.NoError(t, err)
	var found bool
	for _, event := range events {
		if event.Type == "issue.updated" {
			require.Contains(t, event.Payload, `"attributed_teammate":"worker-1"`)
			found = true
		}
	}
	require.True(t, found)
}

func TestTeammateInterruptedCloseContribution(t *testing.T) {
	for _, original := range []string{"reviewer-1", ""} {
		t.Run("original="+original, func(t *testing.T) {
			env, dir, pid, ref := setupWorkspaceWithIssue(t, "Retained close")
			t.Setenv("KATA_TEAMMATE", original)
			args := []string{"close", ref, "--done", "--message", "Implemented the requested behavior and ran focused tests.", "--test", "go test ./internal/example", "--idempotency-key", "retained-close"}
			// Recreate the committed-close / missing-followup state of an interrupted command.
			runCLI(t, env, dir, args...)
			t.Setenv("KATA_TEAMMATE", "new-worker")
			runCLI(t, env, dir, append(args, "--comment", "Recovered followup")...)
			events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: pid, Limit: 100})
			require.NoError(t, err)
			var found bool
			for _, event := range events {
				if event.Type != "issue.commented" {
					continue
				}
				found = true
				var payload map[string]any
				require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
				if original == "" {
					require.NotContains(t, payload, "attributed_teammate")
				} else {
					require.Equal(t, original, payload["attributed_teammate"])
				}
				require.Equal(t, "new-worker", payload["teammate"], "explicit comment addressing remains main's")
			}
			require.True(t, found)
		})
	}
}

func TestTeammateOldDaemonMutation(t *testing.T) {
	t.Setenv("KATA_TEAMMATE", "worker-1")
	var writes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.17.0"}`))
			return
		}
		if r.URL.Path == "/api/v1/projects/resolve" {
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
			return
		}
		if r.Method != http.MethodGet {
			writes++
			require.Equal(t, "worker-1", r.Header.Get("X-Kata-Teammate"))
		}
		_, _ = w.Write([]byte(`{"issue":{"id":1,"project_id":1,"short_id":"abc1"},"changed":true}`))
	}))
	defer server.Close()
	_, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), "--project", "example-project", "edit", "abc1", "--title", "Updated")
	require.NoError(t, err)
	require.Equal(t, 1, writes)
}
