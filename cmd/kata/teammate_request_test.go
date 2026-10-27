package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
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

func TestTeammateInvalidExplicitFlagClassification(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "Existing issue")
	_, err := runCLICapture(t, env, dir, "edit", ref, "--title", "Updated", "--teammate=bad/handle")
	ce := requireCLIError(t, err, ExitValidation)
	require.Equal(t, kindValidation, ce.Kind)
	require.Contains(t, ce.Message, "teammate")
}

func TestTeammateInvalidResolutionFrozen(t *testing.T) {
	t.Setenv("KATA_TEAMMATE", "bad/handle")
	cmd := newRootCmd()
	cmd.SetContext(context.Background())
	first, firstErr := resolveTeammate(cmd)
	require.Error(t, firstErr)
	t.Setenv("KATA_TEAMMATE", "worker-2")
	second, secondErr := resolveTeammate(cmd)
	require.Equal(t, first, second)
	require.Same(t, firstErr, secondErr)
	_, present := teammate.FromRequest(cmd.Context())
	require.False(t, present, "invalid optional default must not become request contribution")
}

// A dry close has no source event, so its follow-up retains the selected contribution.
func TestTeammateNoSourceCloseFollowupContribution(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		name := "unkeyed"
		if keyed {
			name = "keyed"
		}
		t.Run(name, func(t *testing.T) {
			env, dir, pid, ref := setupWorkspaceWithIssue(t, "Dry-close contribution")
			t.Setenv("KATA_TEAMMATE", "worker-1")
			args := []string{"close", ref, "--done", "--message", "Implemented the requested behavior and ran focused checks.", "--test", "go test ./internal/example", "--dry", "--comment", "Validation note"}
			if keyed {
				args = append(args, "--idempotency-key", "dry-close-1")
			}
			runCLI(t, env, dir, args...)
			got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
			require.Equal(t, "open", got.Issue.Status)
			require.Len(t, got.Comments, 1, "existing CLI still posts the dry-run follow-up")
			require.Equal(t, "worker-1", got.Comments[0].Teammate, "explicit identity is independent")
			events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: pid, Limit: 100})
			require.NoError(t, err)
			found := false
			for _, event := range events {
				if event.Type == "issue.closed" {
					t.Fatal("dry close must not create a close event")
				}
				if event.Type != "issue.commented" {
					continue
				}
				found = true
				var payload map[string]any
				require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
				t.Logf("keyed=%v comment payload=%s", keyed, event.Payload)
				require.Equal(t, "worker-1", payload["attributed_teammate"], "a fresh current-daemon follow-up has no original close selection to replace the selected contribution")
			}
			require.True(t, found)
		})
	}
}
