package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	for _, tc := range []struct {
		name                 string
		appOpenedAlreadySent bool
		wantReports          []string
	}{
		{name: "both reports share the wait", wantReports: []string{"issue_closed", "app_opened"}},
		{name: "app_opened already sent", appOpenedAlreadySent: true, wantReports: []string{"issue_closed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			handlerErrors := make(chan error, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/ui/telemetry" {
					var body struct {
						Event string `json:"event"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						handlerErrors <- fmt.Errorf("decode telemetry request: %w", err)
						http.Error(w, "invalid telemetry request", http.StatusBadRequest)
						return
					}
					reports <- report{event: body.Event, at: time.Now()}
					<-r.Context().Done()
					return
				}
				r.Header["X-Forwarded-For"] = nil
				proxy.ServeHTTP(w, r)
			}))
			defer server.Close()
			env.URL = server.URL
			if tc.appOpenedAlreadySent {
				day := cliUseNow().UTC().Format(time.DateOnly)
				recordCLIUseDay(cliUseMarkerPath(client.ResolvedDaemon{BaseURL: server.URL}), day)
			}

			resetFlags(t)
			cmd := newRootCmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{
				"--workspace", dir, "close", ref, "--done", "--message",
				"Completed the requested work and checked the result.", "--test", "go test ./example",
			})
			ctx := contextWithBaseURL(context.Background(), env.URL)
			cmd.SetContext(ctx)
			type commandResult struct {
				err error
			}
			result := make(chan commandResult, 1)
			started := time.Now()
			go func() {
				result <- commandResult{err: executeCLICommand(ctx, cmd)}
			}()

			seen := make(map[string]time.Time, len(tc.wantReports))
			seenEvents := make([]string, 0, len(tc.wantReports))
			for range tc.wantReports {
				select {
				case got := <-reports:
					seen[got.event] = got.at
					seenEvents = append(seenEvents, got.event)
				case err := <-handlerErrors:
					t.Fatal(err)
				case <-time.After(time.Second):
					t.Fatal("telemetry report did not start")
				}
			}
			assert.ElementsMatch(t, tc.wantReports, seenEvents)
			if !tc.appOpenedAlreadySent {
				delta := seen["app_opened"].Sub(seen["issue_closed"])
				if delta < 0 {
					delta = -delta
				}
				assert.Less(t, delta, cliUseReportTimeout/2,
					"both reports should start within the shared wait window")
			}

			var got commandResult
			select {
			case got = <-result:
			case err := <-handlerErrors:
				t.Fatal(err)
			case <-time.After(time.Second):
				t.Fatal("close command did not finish after the shared reporting deadline")
			}
			require.NoError(t, got.err)
			assert.Empty(t, stderr.String())
			assert.NotContains(t, stdout.String(), "telemetry")
			assert.GreaterOrEqual(t, time.Since(started), cliUseReportTimeout,
				"the command must wait for its active report")
			select {
			case err := <-handlerErrors:
				t.Fatal(err)
			default:
			}
			select {
			case extra := <-reports:
				t.Fatalf("unexpected telemetry report %q", extra.event)
			default:
			}
		})
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
