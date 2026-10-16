package main

import (
	"bytes"
	"encoding/json"
	"io"
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
	for _, fail := range []bool{false, true} {
		name := "reports changed actions"
		if fail {
			name = "keeps command output when telemetry fails"
		}
		t.Run(name, func(t *testing.T) {
			capture := &cliUseTelemetry{fail: fail}
			env, dir, pid := setupCLIWorkspaceOptions(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = capture })
			t.Setenv(hooks.HookVersionEnv, "")
			stubIsTTY(t, true)
			t.Cleanup(func() { cliUseTarget.Store(nil) })
			createArgs := []string{"--workspace", dir, "create", "Example task", "--idempotency-key", "create-key"}
			output, err := runCmdOutput(t, env, createArgs...)
			require.NoError(t, err)
			assert.Contains(t, output, "Example task")
			_, err = runCmdOutput(t, env, createArgs...)
			require.NoError(t, err)

			ref := createIssue(t, env, pid, "Task to close")
			closeArgs := []string{
				"--workspace", dir, "close", ref, "--done", "--message",
				"Completed the requested work and checked the result.", "--test", "go test ./example",
			}
			output, err = runCmdOutput(t, env, closeArgs...)
			require.NoError(t, err)
			assert.Contains(t, output, "closed")
			_, err = runCmdOutput(t, env, closeArgs...)
			require.NoError(t, err)

			var actions []string
			for _, event := range capture.events {
				if event == "issue_created" || event == "issue_closed" {
					actions = append(actions, event)
				}
			}
			assert.Equal(t, []string{"issue_created", "issue_closed"}, actions)
			for i, event := range capture.events {
				if event == "issue_created" || event == "issue_closed" {
					assert.Equal(t, map[string]any{"surface": "cli"}, capture.recorded()[i])
				}
			}
		})
	}
}

func TestCLICoreActionsDryRunDoesNotReport(t *testing.T) {
	capture := &cliUseTelemetry{}
	env, dir, pid := setupCLIWorkspaceOptions(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = capture })
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	ref := createIssue(t, env, pid, "Task to close")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	var dryRunStatus int
	var dryRunBody []byte
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request != nil && response.Request.URL.Path == "/api/v1/projects/"+itoa(pid)+"/issues/"+ref+"/actions/close" {
			dryRunStatus = response.StatusCode
			body, err := io.ReadAll(response.Body)
			if err != nil {
				return err
			}
			dryRunBody = body
			response.Body = io.NopCloser(bytes.NewReader(body))
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	env.URL = server.URL
	args := []string{
		"--workspace", dir, "close", ref, "--done", "--message",
		"Completed the requested work and checked the result.", "--test", "go test ./example", "--dry",
	}
	_, err = runCmdOutput(t, env, args...)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, dryRunStatus)
	var response struct {
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(dryRunBody, &response))
	assert.False(t, response.Changed)
	assert.NotContains(t, capture.events, "issue_closed")
}

func TestCLICloseReportsBeforeFailingComment(t *testing.T) {
	capture := &cliUseTelemetry{}
	env, dir, pid := setupCLIWorkspaceOptions(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = capture })
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	ref := createIssue(t, env, pid, "Task to close")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	commentReached := make(chan struct{})
	var expired atomic.Bool
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
	_, err = runCmdOutput(t, env, "--workspace", dir, "close", ref, "--done", "--message", "Completed the requested work and checked the result.", "--test", "go test ./example", "--comment", "Follow-up note")
	require.ErrorContains(t, err, "Comment failed")
	assert.False(t, expired.Load(), "the comment must proceed while telemetry is pending")
	assert.Contains(t, capture.events, "issue_closed")
}
