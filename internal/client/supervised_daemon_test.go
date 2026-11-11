package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

func TestManagedExternallyDoesNotAutostart(t *testing.T) {
	for _, defaultLimit := range []bool{false, true} {
		t.Run(fmt.Sprintf("default_limit=%t", defaultLimit), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// The default context has no deadline: the documented limit is 30s.
				deadline, waited := 100*time.Millisecond, 100*time.Millisecond
				if defaultLimit {
					deadline, waited = 0, 30*time.Second
				}
				home := setupKataEnv(t)
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n[auth]\ntoken_env = 'SUPERVISOR_ONLY_TOKEN'\n"), 0600))
				state := patchEnsureHooks(t, "v0.2.0", "http://spawned")
				ctx := t.Context()
				if deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, deadline)
					defer cancel()
				}
				start := time.Now()
				_, err := ensureLocalRunningTarget(ctx)
				require.ErrorContains(t, err, "managed externally")
				require.Equal(t, waited, time.Since(start))
				require.Zero(t, state.startCalls)
				require.Zero(t, state.stopCalls)
				if deadline > 0 {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
			})
		})
	}
}

func TestManagedExternallyUnreachableDetail(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear=%t", clear), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cause := fmt.Errorf("daemon endpoint unreachable: %w", ErrLocalDaemonUnreachable)
				calls := 0
				scan := func(context.Context) (ensureDiscovery, error) {
					calls++
					if clear && calls > 1 {
						return ensureDiscovery{}, nil
					}
					return ensureDiscovery{}, cause
				}
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				_, err := waitForSupervisedDaemon(ctx, "/example/config.toml", scan)
				require.ErrorIs(t, err, ErrDaemonManagedExternally)
				if clear {
					require.NotErrorIs(t, err, cause)
				} else {
					require.ErrorIs(t, err, cause)
					require.ErrorContains(t, err, "service manager")
					require.ErrorContains(t, err, "autostart = false")
				}
			})
		})
	}
}

func TestManagedExternallyProfileDoesNotAutostart(t *testing.T) {
	for _, oldSchema := range []bool{false, true} {
		t.Run(fmt.Sprintf("old_schema=%t", oldSchema), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				profile, root := localProfileFixture(t)
				require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n"), 0600))
				if oldSchema {
					stageStorageSchema(t, filepath.Join(profile.Home, "kata.db"), db.CurrentSchemaVersion()-1)
				}
				selection, err := InspectSelection(t.Context(), root, "")
				require.NoError(t, err)
				orig := startDetachedDaemonForEnsure
				t.Cleanup(func() { startDetachedDaemonForEnsure = orig })
				starts := 0
				startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error {
					starts++
					return errors.New("unexpected spawn")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				_, err = ensureLocalProfile(ctx, selection)
				require.ErrorIs(t, err, ErrDaemonManagedExternally)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Zero(t, starts)
			})
		})
	}
}

func TestManagedExternallyDoesNotWaitForStartLock(t *testing.T) {
	home := setupKataEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n"), 0600))
	server, pings := restartGapDaemon(t, "", "")
	require.NoError(t, writeRuntimeRecord(t, home, strings.TrimPrefix(server.URL, "http://")))
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	release, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).AcquireStartLock(t.Context())
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	running, err := ensureLocalRunningTarget(ctx)
	require.NoError(t, err)
	require.Equal(t, server.URL, running.BaseURL)
	require.GreaterOrEqual(t, pings.Load(), int32(2))
}

// A current home's runtime directory that has lost owner-only permissions is
// repaired by ordinary discovery, as it is when autostart is enabled.
func TestManagedExternallyRepairsCurrentHomeRuntimeDirectory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		home := setupKataEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n"), 0600))
		ns, err := daemonruntime.NewNamespace()
		require.NoError(t, err)
		makeRuntimeDirectoryInsecureForTest(t, ns.DataDir)
		state := patchEnsureHooks(t, "v0.2.0", "http://spawned")
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err = ensureLocalRunningTarget(ctx)
		require.ErrorIs(t, err, ErrDaemonManagedExternally)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NoError(t, safefileio.ValidatePrivateDir(ns.DataDir))
		require.Zero(t, state.startCalls)
	})
}

// restartGapDaemon is a live daemon whose first ping fails, as during a
// supervisor restart. Its instance endpoint requires the bearer token, if any.
func restartGapDaemon(t *testing.T, uid, token string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	pings := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/instance" && r.Header.Get("Authorization") != "Bearer "+token:
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/api/v1/instance":
			_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
		case pings.Add(1) == 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":%q,"pid":%d}`, currentVersionForEnsure(), os.Getpid())
		}
	}))
	t.Cleanup(server.Close)
	return server, pings
}

func TestExternallyManagedVersionErrorGuidesByDirection(t *testing.T) {
	patchEnsureHooks(t, "v0.2.0", "http://spawned")
	err := externallyManagedVersionError(PingInfo{Version: "v0.1.0"})
	require.ErrorIs(t, err, ErrDaemonNotReplaceable)
	require.ErrorContains(t, err, "v0.1.0")
	require.ErrorContains(t, err, "v0.2.0")
	require.ErrorContains(t, err, "service manager")
	err = externallyManagedVersionError(PingInfo{Version: "v0.3.0"})
	require.ErrorIs(t, err, ErrDaemonNotReplaceable)
	require.ErrorContains(t, err, "use a newer kata binary")
}

// The daemon's own auth.token_env is supervisor-owned: selection must accept it
// unresolved beside a separate catalog credential, and the live wait must send
// that catalog credential, retry a transport failure and still check identity.
func TestSupervisedProfileCatalogCredentialAuthenticatesLiveDaemon(t *testing.T) {
	profile, root := localProfileFixture(t)
	t.Setenv("KATA_AUTH_TOKEN", "")
	require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n[auth]\ntoken_env = 'KATA_AUTH_TOKEN'\n"), 0600))
	configPath := filepath.Join(os.Getenv("KATA_HOME"), "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("[[daemon]]\nname = 'work'\nlocal = true\nhome = %q\ninstance_uid = %q\ntoken = 'client-token'\n", profile.Home, profile.InstanceUID)), 0600)) //nolint:gosec // G703: configPath is the test's own KATA_HOME catalog.
	selection, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	ns, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, safefileio.EnsurePrivateDir(ns.DataDir)) // profile directories are validated, never repaired
	server, pings := restartGapDaemon(t, profile.InstanceUID, "client-token")
	other, _ := restartGapDaemon(t, "01HZZZZZZZZZZZZZZZZZZZZZ01", "client-token")
	publish := func(daemon *httptest.Server) {
		_, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{Service: "kata", PID: os.Getpid(), Address: strings.TrimPrefix(daemon.URL, "http://"), Network: "tcp"})
		require.NoError(t, err)
	}
	publish(server)
	resolved, err := ensureLocalProfile(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, server.URL, resolved.BaseURL)
	require.GreaterOrEqual(t, pings.Load(), int32(2))
	publish(other)
	_, err = ensureLocalProfile(t.Context(), selection)
	require.ErrorIs(t, err, ErrProfileIdentityMismatch)
}
