package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestCronRuntimeWarningsCLI(t *testing.T) {
	for _, action := range []string{"list", "create", "update", "restore"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("KATA_AUTHOR", "operator")
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			project, err := env.DB.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			id := cronCLIUID(t)
			body := cronCLIJob(t)
			body.Definition.Enabled = true
			var current db.CronJob
			if action != "create" {
				current, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: id, Name: body.Name, Actor: "operator", Definition: body.Definition.Native()})
				require.NoError(t, err)
			}
			if action == "restore" {
				current, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: id, Name: current.Name, Actor: "operator", Definition: current.Definition, ExpectedEventUID: current.DefinitionEventUID, Deleted: true})
				require.NoError(t, err)
			}
			inventory := cron.RuntimeInventory{Inventory: cron.RuntimeInventoryState{State: "ready"}, Runtimes: []cron.Runtime{}, Warnings: []cron.RuntimeWarning{{Code: "pin_unmatched", JobUID: id, Message: "pin needs an advertisement"}, {Code: "no_live_runtime", Message: "no live runtime advertisement"}, {Code: "pin_unmatched", JobUID: cronCLIUID(t), Message: "another job warning"}}}
			var calls atomic.Int32
			upstream, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cron/runtimes") {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					raw, e := json.Marshal(inventory)
					require.NoError(t, e)
					_, _ = w.Write(raw)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			var baseline string
			if action == "list" {
				baseline, _, err = executeCronCLI(t, server.URL, nil, "job", "list", "--quiet")
				require.NoError(t, err)
				require.Zero(t, calls.Load(), "quiet performed optional read")
			}
			args := []string{"job", action}
			switch action {
			case "create":
				args = append(args, "--uid", id, "--file", cronCLIFile(t, body))
			case "update":
				args = append(args, id, "--file", cronCLIFile(t, body), "--expected-event-uid", current.DefinitionEventUID)
			case "restore":
				args = append(args, id, "--expected-event-uid", current.DefinitionEventUID)
			}
			out, diagnostic, err := executeCronCLI(t, server.URL, nil, args...)
			require.NoError(t, err)
			require.Equal(t, int32(1), calls.Load())
			require.Contains(t, diagnostic, "warning: pin needs an advertisement (job "+id+"; kata cron runtime list)")
			require.Contains(t, diagnostic, "warning: no live runtime advertisement (kata cron runtime list)")
			if action == "list" {
				require.Equal(t, baseline, out)
				require.Contains(t, diagnostic, "another job warning")
			} else {
				require.NotContains(t, diagnostic, "another job warning")
			}
		})
	}
}
func TestCronRuntimeWarningsUnavailableSilent(t *testing.T) {
	for _, mode := range []string{"404", "500", "warming", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KATA_AUTHOR", "operator")
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			_, err := env.DB.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			upstream, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cron/runtimes") {
					if mode == "404" {
						http.NotFound(w, r)
						return
					}
					if mode == "500" {
						w.WriteHeader(500)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"inventory":{"state":"`+mode+`"},"warnings":[{"code":"no_live_runtime","message":"must stay quiet"}],"runtimes":[]}`)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			_, diagnostic, err := executeCronCLI(t, server.URL, nil, "job", "list")
			require.NoError(t, err)
			require.NotContains(t, diagnostic, "warning:")
		})
	}
}

type runtimeNoticeTransport func(*http.Request) (*http.Response, error)

func (f runtimeNoticeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCronRuntimeWarningsBoundedAndOptional(t *testing.T) {
	resetFlags(t)
	called := 0
	client, err := kataclient.NewWithHTTPClient("http://daemon.example", &http.Client{Transport: runtimeNoticeTransport(func(r *http.Request) (*http.Response, error) {
		called++
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 3*time.Second)
		return nil, io.ErrUnexpectedEOF
	})})
	require.NoError(t, err)
	command := newRootCmd()
	var output bytes.Buffer
	command.SetErr(&output)
	(cronCLIOptions{resource: "job", action: "list"}).warnRuntimes(t.Context(), command, client, 7, nil)
	require.Equal(t, 1, called)
	require.Empty(t, output.String())
	flags.Quiet = true
	(cronCLIOptions{resource: "job", action: "list"}).warnRuntimes(t.Context(), command, client, 7, nil)
	require.Equal(t, 1, called)
}

type runtimeWarningBrokenWriter struct {
	calls int
}

func (w *runtimeWarningBrokenWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, io.ErrClosedPipe
}
func TestCronRuntimeWarningsIgnoreStderrFailure(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	upstream, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var runtimeReads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cron/runtimes") {
			runtimeReads++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"inventory":{"state":"ready"},"runtimes":[],"warnings":[{"code":"no_live_runtime","message":"no live advertisement"}]}`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	baseline, _, err := executeCronCLI(t, server.URL, nil, "job", "list", "--quiet")
	require.NoError(t, err)
	require.Zero(t, runtimeReads, "quiet command performed optional read")

	writer := &runtimeWarningBrokenWriter{}
	var stdout bytes.Buffer
	err = executeCronCLIWithWriters(t, server.URL, nil, &stdout, writer, "job", "list")
	require.NoError(t, err, "a failed diagnostic write must not change the successful command result")
	require.Equal(t, baseline, stdout.String(), "a failed diagnostic write must not change stdout")
	require.Equal(t, 1, runtimeReads)
	require.Equal(t, 1, writer.calls, "the warning should still be attempted on stderr")
}

func TestCronRuntimeChoiceWarningsActions(t *testing.T) {
	resetFlags(t)
	client, err := kataclient.NewWithHTTPClient("http://daemon.example", &http.Client{Transport: runtimeNoticeTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"inventory":{"state":"ready"},"runtimes":[],"warnings":[{"code":"choice_unknown","job_uid":"written","message":"unadvertised value"},{"code":"choice_unknown","job_uid":"other","message":"other value"}]}`)), Request: r}, nil
	})})
	require.NoError(t, err)
	for _, action := range []string{"list", "create", "update", "restore"} {
		cmd := newRootCmd()
		var out bytes.Buffer
		cmd.SetErr(&out)
		(cronCLIOptions{resource: "job", action: action}).warnRuntimes(t.Context(), cmd, client, 7, []byte(`{"job":{"uid":"written"}}`))
		if action == "create" || action == "update" {
			require.Contains(t, out.String(), "unadvertised value")
		} else {
			require.Empty(t, out.String())
		}
		require.NotContains(t, out.String(), "other value")
	}
}
