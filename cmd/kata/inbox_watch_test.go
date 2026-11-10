package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	daemonclient "go.kenn.io/kata/internal/client"
)

// loadInboxWatchSnapshot accepts a complete scan or nothing. It deliberately
// skips one-shot reply enrichment, which tolerates detail-read failures and
// can change rendered text after an unrelated comment changes its handle.
func loadInboxWatchSnapshot(ctx context.Context, cmd *cobra.Command, recipient string, allProjects bool) ([]inboxWatchRequest, error) {
	read, closeIdle, err := newInboxWatchReader(ctx, cmd, recipient, allProjects)
	if err != nil {
		return nil, err
	}
	defer closeIdle()
	return read(ctx)
}

func TestInboxHelpExplainsJSONOutputMayBeStale(t *testing.T) {
	cmd := newInboxCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, cmd.Help())
	require.Contains(t, out.String(), "JSON output can be stale; check current attention before acting.")
}

// The product contract is an immediately available, exact-recipient wakeup
// batch. Reading it must not claim or acknowledge the pending request.
func TestInboxWatchReadsInitialBacklog(t *testing.T) {
	const recipient = "coordinator/worker"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.43.0"}`)
		case "/api/v1/projects/resolve":
			_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"example-project"}}`)
		case "/api/v1/projects/1/issues":
			require.Equal(t, http.MethodGet, r.Method)
			require.Equal(t, notificationMetadataKey(recipient), r.URL.Query().Get("meta"))
			require.Equal(t, "true", r.URL.Query().Get("compact"))
			require.Equal(t, "oldest", r.URL.Query().Get("sort"))
			require.Equal(t, "100", r.URL.Query().Get("limit"))
			require.Equal(t, "open", r.URL.Query().Get("status"))
			_, _ = fmt.Fprintf(w, `{"complete":true,"truncated":false,"issues":[{"uid":"issue-uid","project_uid":"project-uid","short_id":"abcd","title":"Inspect finding","metadata":{%q:{"from":"sender","message":"untrusted\nattention"}}}]}`, notificationMetadataKey(recipient))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	resetFlags(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := newRootCmd()
	var output bytes.Buffer
	cmd.SetOut(inboxCancelWriter{Writer: &output, cancel: cancel})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(contextWithBaseURL(ctx, server.URL))
	cmd.SetArgs([]string{"--project", "example-project", "inbox", "--watch", "--for", recipient, "--json"})
	require.NoError(t, cmd.Execute())
	require.Equal(t, 1, bytes.Count(output.Bytes(), []byte{'\n'}), "JSON watch batches must be NDJSON")
	var batch struct {
		Recipient string `json:"recipient"`
		Requests  []struct {
			UID        string `json:"issue_uid"`
			ProjectUID string `json:"project_uid"`
			Message    string `json:"message"`
		} `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &batch))
	require.Equal(t, recipient, batch.Recipient)
	require.Len(t, batch.Requests, 1)
	require.Equal(t, "issue-uid", batch.Requests[0].UID)
	require.Equal(t, "project-uid", batch.Requests[0].ProjectUID)
	require.Equal(t, "untrusted\nattention", batch.Requests[0].Message)
}

type inboxCancelWriter struct {
	Writer *bytes.Buffer
	cancel context.CancelFunc
}

func (w inboxCancelWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.cancel()
	return n, err
}

func TestInboxWatchRequiresCompactPaginatedAPI(t *testing.T) {
	for _, tc := range []struct {
		name        string
		version     string
		allProjects bool
	}{
		{name: "project scope without pagination", version: "0.26.0"},
		{name: "project scope without compact rows", version: "0.42.0"},
		{name: "all projects without compact rows", version: "0.42.0", allProjects: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(t)
			flags.Project = "example-project"
			var healthCalls, resolveCalls, listCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					healthCalls.Add(1)
					_, _ = fmt.Fprintf(w, `{"ok":true,"api_schema_version":%q}`, tc.version)
				case "/api/v1/projects/resolve":
					resolveCalls.Add(1)
					_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"example-project"}}`)
				case "/api/v1/projects/1/issues", "/api/v1/issues":
					listCalls.Add(1)
					_, _ = fmt.Fprint(w, `{"complete":true,"truncated":false,"issues":[]}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			ctx := contextWithBaseURL(t.Context(), server.URL)
			api := daemonAPI{ctx: ctx, client: server.Client(), baseURL: server.URL}
			_, closeIdle, err := inboxWatchReaderForAPI(ctx, api, newInboxCmd(), "reader", tc.allProjects)
			if closeIdle != nil {
				defer closeIdle()
			}
			cli := requireCLIError(t, err, ExitValidation)
			require.Equal(t, "daemon_api_too_old", cli.Code)
			require.Contains(t, cli.Message, "inbox --watch requires daemon API 0.43.0 or newer")
			require.EqualValues(t, 1, healthCalls.Load())
			require.Zero(t, resolveCalls.Load())
			require.Zero(t, listCalls.Load())
		})
	}
}

func TestInboxWatchDrainsEmptyPagesAndSurfacesOversized(t *testing.T) {
	key := notificationMetadataKey("reader/worker")
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.43.0"}`)
			return
		}
		if r.URL.Path == "/api/v1/projects/resolve" {
			_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"example-project"}}`)
			return
		}
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		switch cursor {
		case "":
			_, _ = fmt.Fprint(w, `{"complete":false,"truncated":true,"next_cursor":"empty","issues":[{"uid":"large","project_uid":"project","short_id":"abcd","oversized":true}]}`)
		case "empty":
			_, _ = fmt.Fprint(w, `{"complete":false,"truncated":true,"next_cursor":"last","issues":[]}`)
		case "last":
			_, _ = fmt.Fprintf(w, `{"complete":true,"truncated":false,"issues":[{"uid":"later","project_uid":"project","short_id":"efgh","metadata":{%q:{"from":"sender","message":"Later request"}}}]}`, key)
		default:
			t.Errorf("unexpected cursor %q", cursor)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	resetFlags(t)
	flags.Project = "example-project"
	cmd := newInboxCmd()
	cmd.SetErr(&bytes.Buffer{})
	ctx := contextWithBaseURL(t.Context(), server.URL)
	requests, err := loadInboxWatchSnapshot(ctx, cmd, "reader/worker", false)
	require.NoError(t, err)
	require.Equal(t, []string{"", "empty", "last"}, cursors)
	require.Len(t, requests, 2)
	require.Equal(t, "large", requests[0].IssueUID)
	require.True(t, requests[0].Oversized)
	require.Equal(t, "later", requests[1].IssueUID)
	require.Equal(t, "Later request", requests[1].Message)
}

func TestInboxWatchDiscardsIncompleteScans(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ending    string
		wantError string
	}{
		{
			name:      "missing cursor",
			ending:    `{"complete":false,"truncated":true,"issues":[]}`,
			wantError: "incomplete or inconsistent results; check daemon compatibility",
		},
		{
			name:      "cursor cycle",
			ending:    `{"complete":false,"truncated":true,"next_cursor":"next","issues":[]}`,
			wantError: "incomplete or inconsistent results; check daemon compatibility",
		},
		{
			name:      "missing identity",
			ending:    `{"complete":true,"truncated":false,"issues":[{"short_id":"no-identity"}]}`,
			wantError: "could not identify a returned issue",
		},
		{
			name:      "missing issue list",
			ending:    `{"complete":true,"truncated":false}`,
			wantError: "incomplete response; check daemon compatibility",
		},
		{
			name:      "null issue list",
			ending:    `{"complete":true,"truncated":false,"issues":null}`,
			wantError: "incomplete response; check daemon compatibility",
		},
		{
			name:      "missing complete field",
			ending:    `{"truncated":true,"next_cursor":"another","issues":[]}`,
			wantError: "incomplete response; check daemon compatibility",
		},
		{
			name:      "missing truncated field",
			ending:    `{"complete":true,"issues":[]}`,
			wantError: "incomplete response; check daemon compatibility",
		},
		{
			name:      "null complete field",
			ending:    `{"complete":null,"truncated":false,"issues":[]}`,
			wantError: "incomplete response; check daemon compatibility",
		},
		{
			name:      "null truncated field",
			ending:    `{"complete":true,"truncated":null,"issues":[]}`,
			wantError: "incomplete response; check daemon compatibility",
		},
		{name: "forbidden", ending: "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.43.0"}`)
					return
				}
				if r.URL.Path == "/api/v1/projects/resolve" {
					_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"example-project"}}`)
					return
				}
				if r.URL.Query().Get("cursor") == "" {
					_, _ = fmt.Fprintf(w, `{"complete":false,"truncated":true,"next_cursor":"next","issues":[{"uid":"first","project_uid":"project","short_id":"abcd","metadata":{%q:{"from":"sender","message":"Do not emit partial scans"}}}]}`, notificationMetadataKey("reader"))
					return
				}
				if tc.ending == "forbidden" {
					w.WriteHeader(http.StatusForbidden)
					_, _ = fmt.Fprint(w, `{"error":{"code":"forbidden","message":"scope removed"}}`)
					return
				}
				_, _ = fmt.Fprint(w, tc.ending)
			}))
			t.Cleanup(server.Close)
			resetFlags(t)
			flags.Project = "example-project"
			requests, err := loadInboxWatchSnapshot(contextWithBaseURL(t.Context(), server.URL), newInboxCmd(), "reader", false)
			require.Error(t, err)
			require.Nil(t, requests)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			}
		})
	}
}

func TestInboxWatchFiltersExactRecipientAgainstDaemon(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "Attention finding")
	for _, recipient := range []string{"coordinator", "coordinator/worker", "coordinator/sibling"} {
		runCLI(t, env, dir, "notify", ref, "--to", recipient, "--message", recipient)
	}
	resetFlags(t)
	flags.Workspace = dir
	requests, err := loadInboxWatchSnapshot(contextWithBaseURL(t.Context(), env.URL), newInboxCmd(), "coordinator/worker", false)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	require.Equal(t, "coordinator/worker", requests[0].Message)
	require.NotEmpty(t, requests[0].IssueUID)
	require.NotEmpty(t, requests[0].ProjectUID)
	// Reading does not acknowledge the exact request or its sibling slots.
	for _, recipient := range []string{"coordinator", "coordinator/worker", "coordinator/sibling"} {
		var snapshot inboxOutput
		require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "inbox", "--for", recipient, "--json")), &snapshot))
		require.Equal(t, recipient, snapshot.Recipient)
		require.Len(t, snapshot.Requests, 1)
		require.Equal(t, recipient, snapshot.Requests[0].Message)
	}
}

func TestInboxWatchRejectsInvalidFlagCombinations(t *testing.T) {
	for _, args := range [][]string{
		{"--interval", "1s"},
		{"--watch", "--interval", "0s"},
		{"--watch", "--interval", "-1s"},
		{"--watch", "--interval", "999ms"},
		{"--watch", "--context"},
		{"--watch", "--all", "--project", "example-project"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			resetFlags(t)
			cmd := newRootCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(append([]string{"inbox", "--for", "reader"}, args...))
			cmd.SetContext(t.Context())
			err := cmd.Execute()
			_ = requireCLIError(t, err, ExitUsage)
		})
	}
}

func TestInboxWatchRendersOversizedHints(t *testing.T) {
	for _, mode := range []outputMode{outputJSON, outputAgent, outputHuman} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			resetFlags(t)
			flags.Mode = mode
			var out bytes.Buffer
			cmd := newInboxCmd()
			cmd.SetOut(&out)
			request := inboxWatchRequest{
				IssueUID: "issue", ProjectUID: "project", Oversized: true,
			}
			request.inboxRequest = inboxRequest{Ref: "abcd"}
			err := printInboxWatch(t.Context(), cmd, "reader", []inboxWatchRequest{request}, false)
			require.NoError(t, err)
			require.Contains(t, out.String(), "abcd")
			require.Contains(t, out.String(), "oversized")
		})
	}
}

// A watcher keeps the daemon and credentials chosen at startup. Re-resolving
// on every scan would both change its destination and allocate new Unix
// transports whose idle connections have no expiry.
func TestInboxWatchPinsSelectedDaemonForItsLifetime(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("version = 1\n[project]\nname = \"example-project\"\n"), 0o600))
	localConfig := filepath.Join(dir, ".kata.local.toml")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var reads atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("watcher followed changed target configuration")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(other.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.43.0"}`)
		case "/api/v1/projects/resolve":
			_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"example-project"}}`)
		case "/api/v1/projects/1/issues":
			if reads.Add(1) == 1 {
				if err := os.WriteFile(localConfig, fmt.Appendf(nil, "version = 1\n[server]\nurl = %q\n", other.URL), 0o600); err != nil {
					t.Error(err)
				}
			} else {
				cancel()
			}
			_, _ = fmt.Fprintf(w, `{"complete":true,"truncated":false,"issues":[{"uid":"issue","project_uid":"project","short_id":"abcd","metadata":{%q:{"from":"sender","message":"pending"}}}]}`, notificationMetadataKey("reader"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	require.NoError(t, os.WriteFile(localConfig, fmt.Appendf(nil, "version = 1\n[server]\nurl = %q\n", server.URL), 0o600))
	flags.Project, flags.Workspace = "example-project", dir
	cmd := newInboxCmd()
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, runInboxWatch(ctx, cmd, "reader", false, 0))
	require.Equal(t, int32(2), reads.Load())
}

func TestInboxWatchReusesUnixConnectionAcrossScans(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not use the Unix-socket daemon transport")
	}
	resetFlags(t)
	flags.Project = "example-project"
	socketDir, err := os.MkdirTemp("", "inbox-uds-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(socketDir)) })
	socket := filepath.Join(socketDir, "daemon.sock")
	listener, err := new(net.ListenConfig).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	var connections atomic.Int32
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				connections.Add(1)
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/projects/resolve" {
				_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"example-project"}}`)
				return
			}
			if r.URL.Path == "/api/v1/health" {
				_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.43.0"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"complete":true,"truncated":false,"issues":[{"uid":"issue","project_uid":"project","short_id":"abcd","metadata":{%q:{"from":"sender","message":"pending"}}}]}`, notificationMetadataKey("reader"))
		}),
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	httpClient, err := daemonclient.NewHTTPClientForUnixSocket(socket, "", daemonclient.Opts{Timeout: 5 * time.Second})
	require.NoError(t, err)
	api := daemonAPI{ctx: t.Context(), client: httpClient, baseURL: daemonclient.UnixBase}
	read, closeIdle, err := inboxWatchReaderForAPI(t.Context(), api, newInboxCmd(), "reader", false)
	require.NoError(t, err)
	defer closeIdle()
	for range 20 {
		requests, err := read(t.Context())
		require.NoError(t, err)
		require.Len(t, requests, 1)
		require.Equal(t, "pending", requests[0].Message)
	}
	require.Equal(t, int32(1), connections.Load(), "resolution and repeated scans must share the owned connection pool")
}

func TestInboxWatchGlobalScopeSkipsMalformedRowsWithoutEnrichment(t *testing.T) {
	resetFlags(t)
	key := notificationMetadataKey("reader/worker")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.99.0"}`)
		case "/api/v1/issues":
			require.Equal(t, key, r.URL.Query().Get("meta"))
			_, _ = fmt.Fprintf(w, `{"complete":true,"truncated":false,"issues":[{"uid":"bad","project_uid":"project","short_id":"bad1","qualified_id":"example-project#bad1","metadata":{%q:{"from":42,"message":"malformed"}}},{"uid":"valid","project_id":7,"project_uid":"project","short_id":"abcd","qualified_id":"example-project#abcd","project_name":"example-project","metadata":{%q:{"from":"sender","message":"stored request","teammate":42,"re":"comment-uid","kind":"reply"}}}]}`, key, key)
		default:
			t.Errorf("unexpected request (including reply enrichment): %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	cmd := newInboxCmd()
	var warnings bytes.Buffer
	cmd.SetErr(&warnings)
	requests, err := loadInboxWatchSnapshot(contextWithBaseURL(t.Context(), server.URL), cmd, "reader/worker", true)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	require.Equal(t, "example-project#abcd", requests[0].Ref)
	require.Equal(t, "example-project", requests[0].Project)
	require.Equal(t, "stored request", requests[0].Message)
	require.Empty(t, requests[0].Teammate)
	require.Contains(t, warnings.String(), "skipped malformed notification")
	require.Contains(t, warnings.String(), "ignored invalid notification teammate")
}

func TestInboxWatchWarnsOnlyWhenMalformedValueChanges(t *testing.T) {
	resetFlags(t)
	flags.Project = "example-project"
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.43.0"}`)
			return
		}
		if r.URL.Path == "/api/v1/projects/resolve" {
			_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"example-project"}}`)
			return
		}
		call := reads.Add(1)
		if call == 6 {
			_, _ = fmt.Fprint(w, `{"complete":true,"truncated":false,"issues":[]}`)
			return
		}
		from, message := "42", "first"
		if call >= 3 {
			message = "changed"
		}
		if call == 4 {
			from = `"sender"`
		}
		_, _ = fmt.Fprintf(w, `{"complete":true,"truncated":false,"issues":[{"uid":"issue","project_uid":"project","short_id":"abcd","title":%q,"metadata":{%q:{"from":%s,"message":%q}}}]}`, fmt.Sprint(call), notificationMetadataKey("reader"), from, message)
	}))
	t.Cleanup(server.Close)
	cmd := newInboxCmd()
	var warnings bytes.Buffer
	cmd.SetErr(&warnings)
	read, closeIdle, err := newInboxWatchReader(contextWithBaseURL(t.Context(), server.URL), cmd, "reader", false)
	require.NoError(t, err)
	defer closeIdle()
	for _, count := range []int{1, 1, 2, 2, 3, 3, 4} {
		_, err := read(t.Context())
		require.NoError(t, err)
		require.Equal(t, count, bytes.Count(warnings.Bytes(), []byte("skipped malformed notification")))
	}
}

func TestInboxWatchCancellationDuringSetupIsClean(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			resetFlags(t)
			t.Setenv("KATA_HOME", t.TempDir())
			t.Setenv("KATA_AUTH_TOKEN", "")
			flags.Project = "example-project"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls.Add(1)
				cancel()
			}))
			t.Cleanup(server.Close)
			if before {
				cancel()
			}
			err := runInboxWatch(contextWithBaseURL(ctx, server.URL), newInboxCmd(), "reader", false, time.Second)
			require.NoError(t, err)
			if before {
				require.Zero(t, calls.Load())
			} else {
				require.Equal(t, int32(1), calls.Load())
			}
		})
	}
}
