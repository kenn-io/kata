package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/hooks"
)

func TestCLIActionAndAppOpenedReportsShareOneDeadline(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	shortenCLIUseReportTimeout(t, 300*time.Millisecond)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "Task to close")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	type report struct {
		event string
		at    time.Time
	}
	reports := make(chan report, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ui/telemetry" {
			var body struct {
				Event string `json:"event"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			reports <- report{event: body.Event, at: time.Now()}
			<-r.Context().Done()
			return
		}
		r.Header["X-Forwarded-For"] = nil
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	env.URL = server.URL

	result := make(chan struct {
		stdout string
		stderr string
		err    error
	}, 1)
	started := time.Now()
	go func() {
		stdout, stderr, err := runCmdCapture(t, env,
			"--workspace", dir, "close", ref, "--done", "--message",
			"Completed the requested work and checked the result.", "--test", "go test ./example",
		)
		result <- struct {
			stdout string
			stderr string
			err    error
		}{stdout: stdout, stderr: stderr, err: err}
	}()

	first := <-reports
	second := <-reports
	seen := map[string]time.Time{first.event: first.at, second.event: second.at}
	assert.Contains(t, seen, "issue_closed")
	assert.Contains(t, seen, "app_opened")
	assert.Less(t, seen["app_opened"].Sub(seen["issue_closed"]), cliUseReportTimeout/2,
		"both reports should start within the shared wait window")

	var got struct {
		stdout string
		stderr string
		err    error
	}
	select {
	case got = <-result:
	case <-time.After(time.Second):
		t.Fatal("close command did not finish after the shared reporting deadline")
	}
	require.NoError(t, got.err)
	assert.Empty(t, got.stderr)
	assert.NotContains(t, got.stdout, "telemetry")
	assert.Less(t, time.Since(started), cliUseReportTimeout+200*time.Millisecond)
}

func TestCLIActionWaitsWhenAppOpenedWasAlreadySent(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	shortenCLIUseReportTimeout(t, 200*time.Millisecond)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "Task to close")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	type report struct {
		event string
		at    time.Time
	}
	reports := make(chan report, 2)
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ui/telemetry" {
			var body struct {
				Event string `json:"event"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			reports <- report{event: body.Event, at: time.Now()}
			<-r.Context().Done()
			close(finished)
			return
		}
		r.Header["X-Forwarded-For"] = nil
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	env.URL = server.URL
	day := cliUseNow().UTC().Format(time.DateOnly)
	recordCLIUseDay(cliUseMarkerPath(client.ResolvedDaemon{BaseURL: server.URL}), day)

	started := time.Now()
	stdout, stderr, err := runCmdCapture(t, env,
		"--workspace", dir, "close", ref, "--done", "--message",
		"Completed the requested work and checked the result.", "--test", "go test ./example",
	)
	elapsed := time.Since(started)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.NotContains(t, stdout, "telemetry")
	select {
	case event := <-reports:
		assert.Equal(t, "issue_closed", event.event)
	case <-time.After(time.Second):
		t.Fatal("issue-action report did not start")
	}
	assert.GreaterOrEqual(t, elapsed, cliUseReportTimeout,
		"the command must join the issue report when app_opened is already recorded")
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("issue-action report did not stop at its deadline")
	}
}

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
