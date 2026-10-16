package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/hooks"
)

func TestCLICoreActionsReportChangedMutations(t *testing.T) {
	for _, failingComment := range []bool{false, true} {
		name := "changed mutations"
		if failingComment {
			name = "failed follow-up comment"
		}
		t.Run(name, func(t *testing.T) {
			capture := &cliUseTelemetry{}
			env, dir, pid := setupCLIWorkspaceOptions(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = capture })
			t.Setenv(hooks.HookVersionEnv, "")
			stubIsTTY(t, true)
			t.Cleanup(func() { cliUseTarget.Store(nil) })
			createArgs := []string{"--workspace", dir, "create", "Example task", "--idempotency-key", "create-key"}
			_, err := runCmdOutput(t, env, createArgs...)
			require.NoError(t, err)
			_, err = runCmdOutput(t, env, createArgs...)
			require.NoError(t, err)

			ref := createIssue(t, env, pid, "Task to close")
			closeArgs := []string{
				"--workspace", dir, "close", ref, "--done", "--message",
				"Completed the requested work and checked the result.", "--test", "go test ./example",
			}
			var expired atomic.Bool
			if failingComment {
				target, err := url.Parse(env.URL)
				require.NoError(t, err)
				proxy := httputil.NewSingleHostReverseProxy(target)
				commentReached := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/ui/telemetry" {
						select {
						case <-commentReached:
						case <-r.Context().Done():
							expired.Store(true)
							return
						}
					}
					if r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+itoa(pid)+"/issues/"+ref+"/comments" {
						close(commentReached)
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = w.Write([]byte(`{"error":{"code":"comment_failed","message":"Comment failed"}}`))
						return
					}
					r.Header["X-Forwarded-For"] = nil
					proxy.ServeHTTP(w, r)
				}))
				defer server.Close()
				env.URL = server.URL
				closeArgs = append(closeArgs, "--comment", "Follow-up note")
			}
			_, err = runCmdOutput(t, env, closeArgs...)
			if failingComment {
				require.ErrorContains(t, err, "Comment failed")
				assert.False(t, expired.Load(), "the comment must proceed while telemetry is pending")
			} else {
				require.NoError(t, err)
				_, err = runCmdOutput(t, env, closeArgs...)
				require.NoError(t, err)
			}

			var actions []string
			for i, event := range capture.recordedEvents() {
				if event == "issue_created" || event == "issue_closed" {
					actions = append(actions, event)
					assert.Equal(t, map[string]any{"surface": "cli"}, capture.recorded()[i])
				}
			}
			assert.Equal(t, []string{"issue_created", "issue_closed"}, actions)
		})
	}
}
