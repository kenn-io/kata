package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

func TestReadOnlyDiscoveryDoesNotCreateRuntimeDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	_, found, err := DiscoverResolvedReadOnly(t.Context(), path)
	require.NoError(t, err)
	require.False(t, found)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "discovery must not materialize runtime state")
}

func TestNormalDiscoveryPreservesRuntimeDirectoryPreparation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	_, found, err := Discover(t.Context(), path)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, safefileio.ValidatePrivateDir(path))
}

func TestDiscoveryRepairsExistingRuntimeDirectoryPrivacyWithoutChangingReadOnlyScan(t *testing.T) {
	dir := t.TempDir()
	assertUnchanged := makeRuntimeDirectoryInsecureForTest(t, dir)

	_, err := readRuntimeRecords(dir)
	require.Error(t, err, "the read-only reader must reject a runtime directory with unsafe permissions")
	assertUnchanged()

	_, found, err := Discover(t.Context(), dir)
	require.NoError(t, err, "normal discovery must preserve RuntimeStore.List's permission repair")
	require.False(t, found)
	require.NoError(t, safefileio.ValidatePrivateDir(dir), "normal discovery must repair the runtime directory")
}

func TestReadRuntimeRecordsRecognizesKitFieldsAndLargeRecords(t *testing.T) {
	for _, body := range []string{
		`{"PID":123,"Address":"127.0.0.1:7777"}`,
		fmt.Sprintf(`{"pid":123,"address":"127.0.0.1:7777","metadata":{"example":%q}}`, strings.Repeat("x", (1<<20)+1)),
	} {
		dir := t.TempDir()
		require.NoError(t, safefileio.EnsurePrivateDir(dir))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "daemon.123.json"), []byte(body), 0600))
		want, err := (kitdaemon.RuntimeStore{Dir: dir}).List()
		require.NoError(t, err)
		require.Len(t, want, 1)
		got, err := readRuntimeRecords(dir)
		require.NoError(t, err)
		require.Equal(t, want, got, "doctor and normal discovery must recognize the same runtime record")
	}
}

func TestDiscoveryRedirectPolicyIsSpecificToDoctor(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			http.Redirect(w, r, "/ping", http.StatusFound)
			return
		}
		redirected.Add(1)
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"test"}`)
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	_, err := (kitdaemon.RuntimeStore{Dir: dir}).Write(kitdaemon.NewRuntimeRecord("kata", "test", kitdaemon.Endpoint{
		Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://"),
	}))
	require.NoError(t, err)
	_, found, err := Discover(t.Context(), dir)
	require.NoError(t, err, "normal discovery still follows same-origin redirects")
	require.True(t, found)
	require.EqualValues(t, 1, redirected.Load())
	_, found, err = DiscoverResolvedReadOnly(t.Context(), dir)
	require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
	require.False(t, found)
	require.EqualValues(t, 1, redirected.Load(), "doctor discovery must not follow the redirect")
}

func TestReadRuntimeRecordsFindsKitRuntimeStoreRecord(t *testing.T) {
	dir := t.TempDir()
	store := kitdaemon.RuntimeStore{Dir: dir}
	written, err := store.Write(kitdaemon.NewRuntimeRecord("kata", "test", kitdaemon.Endpoint{
		Network: "tcp", Address: "127.0.0.1:7777",
	}))
	require.NoError(t, err)

	records, err := readRuntimeRecords(dir)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, os.Getpid(), records[0].PID)
	require.Equal(t, "127.0.0.1:7777", records[0].Address)
	require.Equal(t, written, records[0].SourcePath)
}

// The issue contract is evidence capture and scoped recovery for a live process
// whose listener cannot answer ping; this fixture does not infer the hang's cause.
func TestLiveUnreachableRecoveryAdvice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(server.Close)
	for _, tc := range []struct{ name, origin, want string }{
		{"valid origin", server.URL, server.URL},
		{"malformed origin", "http://user@host/path", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
			require.True(t, ok)
			_, err := (kitdaemon.RuntimeStore{Dir: dir}).Write(kitdaemon.RuntimeRecord{ProcessIdentityV2: identity, Service: "kata", PID: os.Getpid(), Network: "tcp", Address: strings.TrimPrefix(server.URL, "http://"), Metadata: map[string]string{"web_origin": tc.origin, "web_origin_stable": "true", "web_capabilities": "board"}})
			require.NoError(t, err)
			_, found, err := DiscoverResolvedReadOnly(t.Context(), dir)
			require.False(t, found)
			require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
			var observed *LocalDaemonUnreachableError
			require.ErrorAs(t, err, &observed)
			require.Contains(t, err.Error(), fmt.Sprintf("daemon pid %d", os.Getpid()))
			require.Contains(t, err.Error(), server.Listener.Addr().String())
			require.Equal(t, dir, observed.DataDir)
			require.Equal(t, tc.want, observed.WebOrigin)
			if tc.want == "" {
				require.NotContains(t, err.Error(), "/debug/pprof/")
			} else {
				require.Contains(t, err.Error(), tc.want+"/debug/pprof/goroutine?debug=2")
				require.Contains(t, err.Error(), "Authorization: Bearer")
			}
			require.True(t, observed.VerifiedProcess)
		})
	}
	for _, deadline := range []bool{true, false} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var ctx context.Context
				var cancel context.CancelFunc
				if deadline {
					ctx, cancel = context.WithTimeout(t.Context(), 30*time.Millisecond)
				} else {
					ctx, cancel = context.WithCancel(t.Context())
				}
				defer cancel()
				started := make(chan struct{})
				calls := 0
				reader := func(string) ([]kitdaemon.RuntimeRecord, error) {
					return []kitdaemon.RuntimeRecord{{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:1"}, {PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:2"}}, nil
				}
				probe := func(ctx context.Context, addr string) (string, PingInfo, error) {
					calls++
					if calls == 1 {
						close(started)
					}
					select {
					case <-time.After(150 * time.Millisecond):
						return "http://" + addr, PingInfo{PID: os.Getpid()}, nil
					case <-ctx.Done():
						return "", PingInfo{}, &url.Error{Op: "Get", URL: "http://" + addr + "/api/v1/ping", Err: ctx.Err()}
					}
				}
				if !deadline {
					go func() { <-started; cancel() }()
				}
				dir := t.TempDir()
				var errs []error
				for _, err := range liveDaemonsUsing(ctx, dir, reader, probe) {
					errs = append(errs, err)
				}
				require.Len(t, errs, 1)
				require.Equal(t, 1, calls)
				want := context.Canceled
				if deadline {
					want = context.DeadlineExceeded
				}
				require.ErrorIs(t, errs[0], want)
				require.NotErrorIs(t, errs[0], ErrLocalDaemonUnreachable)
				var evidence *LocalDaemonUnreachableError
				require.ErrorAs(t, errs[0], &evidence)
				require.Equal(t, os.Getpid(), evidence.PID)
				require.Equal(t, dir, evidence.DataDir)
				require.Contains(t, errs[0].Error(), fmt.Sprintf("%q", filepath.Join(dir, "daemon.log")))
				require.Contains(t, errs[0].Error(), fmt.Sprint(os.Getpid()))
				for _, text := range []string{"kill", "restart", "service manager", "daemon recover"} {
					require.NotContains(t, errs[0].Error(), text)
				}
			})
		})
	}
}

func TestUnreachableErrorRelaunchPaths(t *testing.T) {
	home := setupKataEnv(t)
	for _, tc := range []struct {
		name                                          string
		managed, auto, profile, migration, unverified bool
		want                                          string
	}{
		{"manual", false, false, false, false, false, "start it again the way you originally started it (same options and environment)"},
		{"automatic", false, true, false, false, false, "rerun the command that failed"},
		{"managed", true, false, false, false, false, "service manager"},
		{"profile", false, false, true, false, false, "kata --daemon work daemon recover"},
		{"migration", false, false, false, true, false, "Wait for the migration"},
		{"unverified", false, false, false, false, true, "unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.managed {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n"), 0600))
				t.Cleanup(func() { require.NoError(t, os.Remove(filepath.Join(home, "config.toml"))) })
			}
			record := kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:1", Metadata: map[string]string{}}
			if tc.unverified {
				record.ProcessIdentityV2 = "future-v1:12345"
			}
			if tc.migration {
				record.Metadata["db_path"] = filepath.Join(dir, "kata.db")
				lock := flock.New(record.Metadata["db_path"] + ".import.lock")
				require.NoError(t, lock.Lock())
				t.Cleanup(func() { require.NoError(t, lock.Close()) })
			}
			if tc.auto {
				record.Metadata[daemonruntime.RuntimeAutoStartKey] = "1"
			}
			e := newUnreachable(t.Context(), dir, record, errors.New("failed"))
			if tc.profile {
				annotateUnreachable(e, ResolvedDaemon{Name: "work", profileConfig: &config.LocalProfileConfig{Config: &config.DaemonConfig{}}})
			}
			text := e.Error()
			require.NotContains(t, text, "kata daemon restart")
			require.Contains(t, text, fmt.Sprintf("%q", filepath.Join(dir, "daemon.log")))
			require.Contains(t, text, fmt.Sprint(os.Getpid()))
			want := tc.want
			if tc.profile && runtime.GOOS == "windows" {
				want = "'--daemon' 'work' 'daemon' 'recover'"
			}
			require.Contains(t, text, want)
			if tc.managed || tc.migration || tc.unverified {
				require.NotContains(t, text, "kill -QUIT")
				require.NotContains(t, text, "taskkill")
				return
			}
			if runtime.GOOS == "windows" {
				require.Contains(t, text, "taskkill")
			} else {
				require.Contains(t, text, "kill -QUIT")
			}
			_, afterKill, ok := strings.Cut(text, ". Then ")
			require.True(t, ok)
			require.NotContains(t, afterKill, "daemon restart")
			for _, other := range []string{"start it again the way you originally started it (same options and environment)", "rerun the command that failed", "service manager", "kata --daemon work daemon recover"} {
				if other != tc.want {
					require.NotContains(t, afterKill, other)
				}
			}
		})
	}
}
