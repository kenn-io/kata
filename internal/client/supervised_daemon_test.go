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
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestManagedExternallyDoesNotAutostart(t *testing.T) {
	// A zero deadline keeps the default context: the documented limit is 30s.
	for _, tc := range []struct{ deadline, waited time.Duration }{
		{100 * time.Millisecond, 100 * time.Millisecond}, {0, 30 * time.Second},
	} {
		t.Run(tc.waited.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				home := setupKataEnv(t)
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n[auth]\ntoken_env = 'SUPERVISOR_ONLY_TOKEN'\n"), 0600))
				state := patchEnsureHooks(t, "v0.2.0", "http://spawned")
				ctx := t.Context()
				if tc.deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.deadline)
					defer cancel()
				}
				start := time.Now()
				_, err := ensureLocalRunningTarget(ctx)
				require.ErrorContains(t, err, "managed externally")
				require.Equal(t, tc.waited, time.Since(start))
				require.Zero(t, state.startCalls)
				require.Zero(t, state.stopCalls)
				if tc.deadline > 0 {
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
	synctest.Test(t, func(t *testing.T) {
		home := setupKataEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n"), 0600))
		ns, err := daemonruntime.NewNamespace()
		require.NoError(t, err)
		release, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).AcquireStartLock(t.Context())
		require.NoError(t, err)
		defer release()
		orig := discoverDaemonForSupervised
		t.Cleanup(func() { discoverDaemonForSupervised = orig })
		calls := 0
		discoverDaemonForSupervised = func(context.Context, string) (ensureDiscovery, error) {
			calls++
			if calls == 1 {
				return ensureDiscovery{}, nil
			}
			return compatibleEnsureDiscovery("127.0.0.1:7777"), nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		running, err := ensureLocalRunningTarget(ctx)
		require.NoError(t, err)
		require.Equal(t, "http://127.0.0.1:7777", running.BaseURL)
		require.Equal(t, 2, calls)
	})
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
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("[[daemon]]\nname = 'work'\nlocal = true\nhome = %q\ninstance_uid = %q\ntoken = 'client-token'\n", profile.Home, profile.InstanceUID)), 0600))
	selection, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	ns, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	serve := func(uid string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer client-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, uid)
		}))
	}
	server, other := serve(profile.InstanceUID), serve("01HZZZZZZZZZZZZZZZZZZZZZ01")
	defer server.Close()
	defer other.Close()
	target := server
	orig := discoverDaemonForSupervised
	t.Cleanup(func() { discoverDaemonForSupervised = orig })
	calls := 0
	discoverDaemonForSupervised = func(context.Context, string) (ensureDiscovery, error) {
		calls++
		address := strings.TrimPrefix(target.URL, "http://")
		if calls == 1 {
			address = "127.0.0.1:1"
		}
		return ensureDiscovery{Outcome: daemonScanCompatible, Daemon: liveDaemon{BaseURL: "http://" + address, Record: kitdaemon.RuntimeRecord{Address: address, Network: "tcp"}}}, nil
	}
	resolved, err := ensureLocalProfile(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, server.URL, resolved.BaseURL)
	require.Equal(t, 2, calls)
	target = other
	_, err = ensureLocalProfile(t.Context(), selection)
	require.ErrorIs(t, err, ErrProfileIdentityMismatch)
}
