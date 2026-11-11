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

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestManagedExternallyDoesNotAutostart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		home := setupKataEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n[auth]\ntoken_env = 'SUPERVISOR_ONLY_TOKEN'\n"), 0600))
		state := patchEnsureHooks(t, "v0.2.0", "http://spawned")
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err := ensureLocalRunningTarget(ctx)
		require.ErrorContains(t, err, "managed externally")
		require.Zero(t, state.startCalls)
		require.Zero(t, state.stopCalls)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestManagedExternallyUnreachableDetail(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear=%t", clear), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cause := fmt.Errorf("daemon endpoint unreachable: %w", ErrLocalDaemonUnreachable)
				calls := 0
				scan := func(context.Context, string) (ensureDiscovery, error) {
					calls++
					if clear && calls > 1 {
						return ensureDiscovery{}, nil
					}
					return ensureDiscovery{}, cause
				}
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				_, err := waitForSupervisedDaemon(ctx, t.TempDir(), "/example/config.toml", false, scan)
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

func TestManagedExternallyStaleWaitDoesNotReplace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		patchEnsureHooks(t, "v0.2.0", "http://spawned")
		scan := func(context.Context, string) (ensureDiscovery, error) {
			return ensureDiscovery{Outcome: daemonScanStale, Daemon: liveDaemon{Info: PingInfo{Service: "kata", Version: "v0.1.0"}}}, nil
		}
		_, err := waitForSupervisedDaemon(t.Context(), t.TempDir(), "/example/config.toml", false, scan)
		require.ErrorIs(t, err, ErrDaemonNotReplaceable)
		require.ErrorContains(t, err, "v0.1.0")
		require.ErrorContains(t, err, "v0.2.0")
		require.ErrorContains(t, err, "service manager")
	})
}

func TestManagedExternallyCutoverReportsMigration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		home := setupKataEnv(t)
		path := filepath.Join(home, "kata.db")
		lock := flock.New(path + ".import.lock")
		require.NoError(t, lock.Lock())
		t.Cleanup(func() { require.NoError(t, lock.Close()) })
		orig := discoverDaemonForSupervised
		t.Cleanup(func() { discoverDaemonForSupervised = orig })
		discoverDaemonForSupervised = func(context.Context, string) (ensureDiscovery, error) { return ensureDiscovery{}, nil }
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		_, err := waitForSupervisedDaemon(ctx, t.TempDir(), "/example/config.toml", true, discoverDaemonForSupervised)
		require.ErrorContains(t, err, "migration")
		require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
	})
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

func TestManagedExternallyProfileWaitVerifiesIdentity(t *testing.T) {
	profile, root := localProfileFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n"), 0600))
	selection, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	ns, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ01"}`)
	}))
	defer server.Close()
	orig := discoverSupervisedProfile
	t.Cleanup(func() { discoverSupervisedProfile = orig })
	discoverSupervisedProfile = func(context.Context, string) (ensureDiscovery, error) {
		return ensureDiscovery{Outcome: daemonScanCompatible, Daemon: liveDaemon{BaseURL: server.URL, Record: kitdaemon.RuntimeRecord{Address: strings.TrimPrefix(server.URL, "http://"), Network: "tcp"}}}, nil
	}
	_, err = ensureLocalProfile(t.Context(), selection)
	require.ErrorIs(t, err, ErrProfileIdentityMismatch)
}

// A supplied catalog credential lets a client reach its supervised profile
// without possessing the supervisor's private startup token.
func TestSupervisedProfileUsesCatalogCredential(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		profile, root := localProfileFixture(t)
		t.Setenv("SUPERVISOR_ONLY_TOKEN", "")
		require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n[auth]\ntoken_env = 'SUPERVISOR_ONLY_TOKEN'\n"), 0600))
		configPath := filepath.Join(os.Getenv("KATA_HOME"), "config.toml")
		require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("[[daemon]]\nname = 'work'\nlocal = true\nhome = %q\ninstance_uid = %q\ntoken = 'client-token'\n", profile.Home, profile.InstanceUID)), 0600))
		selection, err := InspectSelection(t.Context(), root, "")
		require.NoError(t, err)
		require.Equal(t, "client-token", selection.Resolved.Token)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err = ensureLocalProfile(ctx, selection)
		require.ErrorIs(t, err, ErrDaemonManagedExternally)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

// A daemon lost between its ping and identity response may return before the
// supervisor's readiness deadline; transport failures must be retried.
func TestSupervisedProfileRetriesIdentityTransportFailure(t *testing.T) {
	profile, root := localProfileFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n"), 0600))
	selection, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	ns, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, profile.InstanceUID)
	}))
	defer server.Close()
	orig := discoverSupervisedProfile
	t.Cleanup(func() { discoverSupervisedProfile = orig })
	calls := 0
	discoverSupervisedProfile = func(context.Context, string) (ensureDiscovery, error) {
		calls++
		address := strings.TrimPrefix(server.URL, "http://")
		if calls == 1 {
			address = "127.0.0.1:1"
		}
		return ensureDiscovery{Outcome: daemonScanCompatible, Daemon: liveDaemon{BaseURL: "http://" + address, Record: kitdaemon.RuntimeRecord{Address: address, Network: "tcp"}}}, nil
	}
	resolved, err := ensureLocalProfile(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, server.URL, resolved.BaseURL)
	require.Equal(t, 2, calls)
}

func TestSupervisedNewerDaemonRecommendsNewBinary(t *testing.T) {
	patchEnsureHooks(t, "v0.2.0", "http://spawned")
	err := externallyManagedVersionError(PingInfo{Version: "v0.3.0"})
	require.ErrorIs(t, err, ErrDaemonNotReplaceable)
	require.ErrorContains(t, err, "use a newer kata binary")
}

func TestSupervisedProfileCatalogCredentialAuthenticatesLiveDaemon(t *testing.T) {
	profile, root := localProfileFixture(t)
	t.Setenv("SUPERVISOR_ONLY_TOKEN", "")
	require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n[auth]\ntoken_env = 'SUPERVISOR_ONLY_TOKEN'\n"), 0600))
	configPath := filepath.Join(os.Getenv("KATA_HOME"), "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("[[daemon]]\nname = 'work'\nlocal = true\nhome = %q\ninstance_uid = %q\ntoken = 'client-token'\n", profile.Home, profile.InstanceUID)), 0600))
	selection, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	ns, err := daemonruntime.NewNamespaceForHome(profile.Home, profile.StorageID)
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer client-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"instance_uid":%q}`, profile.InstanceUID)
	}))
	defer server.Close()
	orig := discoverSupervisedProfile
	t.Cleanup(func() { discoverSupervisedProfile = orig })
	discoverSupervisedProfile = func(context.Context, string) (ensureDiscovery, error) {
		return ensureDiscovery{Outcome: daemonScanCompatible, Daemon: liveDaemon{BaseURL: server.URL, Record: kitdaemon.RuntimeRecord{Address: strings.TrimPrefix(server.URL, "http://"), Network: "tcp"}}}, nil
	}
	resolved, err := ensureLocalProfile(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, server.URL, resolved.BaseURL)
	selection.Resolved.Token = "wrong-token"
	_, err = ensureLocalProfile(t.Context(), selection)
	require.ErrorContains(t, err, "authentication failed")
}
