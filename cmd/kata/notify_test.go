package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func notificationMetadata(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	var metadata map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &metadata))
	values := make(map[string]string, len(metadata))
	for key, value := range metadata {
		values[key] = string(value)
	}
	return values
}

func TestNotifyAgentOutputUsesCommandHeader(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "agent output")
	out := runCLIAs(t, env, dir, "agent-a", "--agent", "notify", ref,
		"--to", "reviewer", "--message", "review it")
	assert.Contains(t, out, "OK notify "+ref+" to=reviewer changed=true")
}

func TestNotifyRealDaemonInterleavedIssueChanges(t *testing.T) {
	for _, kind := range []string{"close", "other recipient"} {
		t.Run(kind, func(t *testing.T) {
			env, dir, pid, ref := setupWorkspaceWithIssue(t, "concurrent recipients")
			issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
			require.NoError(t, err)
			target, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(target)
			var interleave sync.Once
			proxy.ModifyResponse = func(response *http.Response) error {
				if response.Request.Method != http.MethodGet ||
					response.Request.URL.Path != "/api/v1/projects/"+itoa(pid)+"/issues/"+ref {
					return nil
				}
				var interleaveErr error
				interleave.Do(func() {
					switch kind {
					case "close":
						closeURL := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", env.URL, pid, ref)
						request, requestErr := http.NewRequestWithContext(context.Background(), http.MethodPost,
							closeURL, strings.NewReader(`{"actor":"agent-b","source":"tui"}`))
						if requestErr != nil {
							interleaveErr = requestErr
							return
						}
						request.Header.Set("Content-Type", "application/json")
						closeResponse, requestErr := env.HTTP.Do(request)
						if requestErr != nil {
							interleaveErr = requestErr
							return
						}
						defer func() { _ = closeResponse.Body.Close() }()
						responseBody, readErr := io.ReadAll(closeResponse.Body)
						if readErr != nil {
							interleaveErr = readErr
							return
						}
						if closeResponse.StatusCode != http.StatusOK {
							interleaveErr = fmt.Errorf("close returned %d: %s", closeResponse.StatusCode, responseBody)
						}
					case "other recipient":
						_, interleaveErr = env.DB.PatchIssueMetadata(context.Background(), db.PatchIssueMetadataIn{
							IssueID: issue.ID,
							Actor:   "agent-b",
							Patch: map[string]json.RawMessage{
								"notify.b3Bz": json.RawMessage(`{"from":"agent-b","message":"check rollout"}`),
							},
						})
					}
				})
				return interleaveErr
			}
			var patchRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/metadata") {
					patchRequests.Add(1)
				}
				proxy.ServeHTTP(w, request)
			}))
			t.Cleanup(server.Close)

			resetFlags(t)
			cmd := newRootCmd()
			cmd.SetContext(contextWithBaseURL(context.Background(), server.URL))
			cmd.SetOut(io.Discard)
			cmd.SetArgs([]string{"--workspace", dir, "--as", "agent-a",
				"notify", ref, "--to", "reviewer", "--message", "review it"})
			notifyResult := cmd.Execute()
			stored, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			metadata := notificationMetadata(t, json.RawMessage(stored.Metadata))
			if kind == "close" {
				require.NoError(t, notifyResult)
				assert.Equal(t, int32(1), patchRequests.Load())
				assert.Equal(t, "closed", stored.Status)
				assert.JSONEq(t, `{"from":"agent-a","message":"review it"}`, metadata["notify.cmV2aWV3ZXI"])
				assert.Empty(t, runCLI(t, env, dir, "inbox", "--for", "reviewer", "--context"))
				_, notifyErr := runCLICapture(t, env, dir, "notify", ref,
					"--to", "reviewer", "--message", "closed update")
				_ = requireCLIError(t, notifyErr, ExitValidation)
				runCLI(t, env, dir, "reopen", ref)
				assert.Contains(t, runCLI(t, env, dir, "inbox", "--for", "reviewer"), "review it")
				return
			}

			cli := requireCLIError(t, notifyResult, ExitConfirm)
			assert.Contains(t, cli.Message, "revision conflict")
			assert.Equal(t, int32(1), patchRequests.Load(), "notify must not retry a stale write")
			assert.NotContains(t, metadata, "notify.cmV2aWV3ZXI")
			assert.JSONEq(t, `{"from":"agent-b","message":"check rollout"}`, metadata["notify.b3Bz"])
			runCLIAs(t, env, dir, "agent-a", "notify", ref,
				"--to", "reviewer", "--message", "review it")
			stored, err = env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			metadata = notificationMetadata(t, json.RawMessage(stored.Metadata))
			assert.JSONEq(t, `{"from":"agent-b","message":"check rollout"}`, metadata["notify.b3Bz"])
			assert.JSONEq(t, `{"from":"agent-a","message":"review it"}`, metadata["notify.cmV2aWV3ZXI"])
		})
	}
}

func issueMetadataJSON(t *testing.T, out string) json.RawMessage {
	t.Helper()
	var response struct {
		Issue struct {
			Metadata json.RawMessage `json:"metadata"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &response))
	return response.Issue.Metadata
}

func TestNotifyStoresReplacesAndClearsOneRecipient(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "review the callback")

	runCLIAs(t, env, dir, "agent-a", "notify", ref,
		"--to", " reviewer ", "--message", "first pass")
	runCLIAs(t, env, dir, "agent-b", "notify", ref,
		"--to", "ops", "--message", "check rollout")
	runCLIAs(t, env, dir, "agent-c", "notify", ref,
		"--to", "reviewer", "--message", "second pass")

	metadata := notificationMetadata(t, issueMetadataJSON(t, runCLI(t, env, dir, "--json", "show", ref)))
	assert.JSONEq(t, `{"from":"agent-c","message":"second pass"}`, metadata["notify.cmV2aWV3ZXI"])
	assert.JSONEq(t, `{"from":"agent-b","message":"check rollout"}`, metadata["notify.b3Bz"])

	runCLI(t, env, dir, "notify", ref, "--to", "reviewer", "--clear")
	metadata = notificationMetadata(t, issueMetadataJSON(t, runCLI(t, env, dir, "--json", "show", ref)))
	assert.NotContains(t, metadata, "notify.cmV2aWV3ZXI")
	assert.Contains(t, metadata, "notify.b3Bz")
}

func TestNotifyRejectsClosedButClearIsAllowed(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "closed request")
	runCLIAs(t, env, dir, "agent-a", "notify", ref,
		"--to", "reviewer", "--message", "look before close")
	runCLIAs(t, env, dir, "agent-a", "close", ref, "--done", "--message",
		"completed the requested work and checked its focused behavior", "--commit", "deadbeef")

	_, err := runCLICapture(t, env, dir, "notify", ref,
		"--to", "reviewer", "--message", "too late")
	cli := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, cli.Message, "closed")

	runCLI(t, env, dir, "notify", ref, "--to", "reviewer", "--clear")
	metadata := notificationMetadata(t, issueMetadataJSON(t, runCLI(t, env, dir, "--json", "show", ref)))
	assert.NotContains(t, metadata, "notify.cmV2aWV3ZXI")
}

func TestNotifyValidatesRecipientAndMessage(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "validation")
	for _, args := range [][]string{
		{"notify", ref, "--to", " ", "--message", "hello"},
		{"notify", ref, "--to", "review\ner", "--message", "hello"},
		{"notify", ref, "--to", strings.Repeat("r", 129), "--message", "hello"},
		{"notify", ref, "--to", "reviewer", "--message", ""},
		{"notify", ref, "--to", "reviewer", "--message", "hello", "--clear"},
	} {
		_, err := runCLICapture(t, env, dir, args...)
		_ = requireCLIError(t, err, ExitValidation)
	}
}
