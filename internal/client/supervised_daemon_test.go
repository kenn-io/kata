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

func TestManagedExternallyWaitReleasesStartLock(t *testing.T) {
	home := setupKataEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n"), 0600))
	state := patchEnsureHooks(t, "v0.2.0", "http://spawned")
	orig := discoverDaemonForAutoStart
	t.Cleanup(func() { discoverDaemonForAutoStart = orig })
	calls := 0
	discoverDaemonForAutoStart = func(_ context.Context, dir string) (ensureDiscovery, error) {
		calls++
		lockPath, err := (kitdaemon.RuntimeStore{Dir: dir}).LockPath()
		require.NoError(t, err)
		lock := flock.New(lockPath)
		defer func() { require.NoError(t, lock.Close()) }()
		locked, err := lock.TryLock()
		require.NoError(t, err)
		require.True(t, locked, "supervisor must be able to acquire start lock")
		if calls < 2 {
			return ensureDiscovery{}, nil
		}
		return ensureDiscovery{Outcome: daemonScanCompatible, Daemon: liveDaemon{BaseURL: "http://supervised"}}, nil
	}
	got, err := ensureLocalRunningTarget(t.Context())
	require.NoError(t, err)
	require.Equal(t, "http://supervised", got.BaseURL)
	require.Equal(t, 2, calls)
	require.Zero(t, state.startCalls)
}

func TestManagedExternallyUnreachableRetainsGuidance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setupKataEnv(t)
		cause := fmt.Errorf("daemon endpoint unreachable: %w", ErrLocalDaemonUnreachable)
		orig := discoverDaemonForAutoStart
		t.Cleanup(func() { discoverDaemonForAutoStart = orig })
		discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) {
			return ensureDiscovery{}, cause
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		_, err := waitForSupervisedDaemon(ctx, t.TempDir(), "/example/config.toml", false)
		require.ErrorIs(t, err, ErrDaemonManagedExternally)
		require.ErrorIs(t, err, cause)
		require.ErrorContains(t, err, "service manager")
		require.ErrorContains(t, err, "autostart = false")
	})
}

func TestManagedExternallyStaleWaitDoesNotReplace(t *testing.T) {
	setupKataEnv(t)
	orig := discoverDaemonForAutoStart
	t.Cleanup(func() { discoverDaemonForAutoStart = orig })
	discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) {
		return ensureDiscovery{Outcome: daemonScanStale, Daemon: liveDaemon{Info: PingInfo{Version: "v0.1.0"}}}, nil
	}
	_, err := waitForSupervisedDaemon(t.Context(), t.TempDir(), "/example/config.toml", false)
	require.ErrorContains(t, err, "v0.1.0")
	require.ErrorContains(t, err, "service manager")
}

func TestManagedExternallyCutoverReportsMigration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		home := setupKataEnv(t)
		path := filepath.Join(home, "kata.db")
		lock := flock.New(path + ".import.lock")
		require.NoError(t, lock.Lock())
		t.Cleanup(func() { require.NoError(t, lock.Close()) })
		orig := discoverDaemonForAutoStart
		t.Cleanup(func() { discoverDaemonForAutoStart = orig })
		discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) { return ensureDiscovery{}, nil }
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := waitForSupervisedDaemon(ctx, t.TempDir(), "/example/config.toml", true)
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "migration"), err.Error())
		require.False(t, errors.Is(err, ErrDaemonManagedExternally))
		require.Less(t, time.Since(started), time.Second)
	})
}

func TestManagedExternallyProfileDoesNotAutostart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		profile, root := localProfileFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n"), 0600))
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
		require.ErrorContains(t, err, "managed externally")
		require.Zero(t, starts)
	})
}

func TestManagedExternallyStaleDoesNotStop(t *testing.T) {
	home := setupKataEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n"), 0600))
	previous := storageInspector
	UseStorageInspector(nil)
	t.Cleanup(func() { UseStorageInspector(previous) })
	_, addr := startMockDaemonPing(t, map[string]any{"ok": true, "service": "kata", "version": "v0.1.0", "pid": os.Getpid()})
	require.NoError(t, writeRuntimeRecord(t, home, addr))
	state := patchEnsureHooks(t, "v0.2.0", "http://spawned")
	_, err := ensureLocalRunningTarget(t.Context())
	require.ErrorContains(t, err, "service manager")
	require.ErrorContains(t, err, "v0.1.0")
	require.ErrorContains(t, err, "v0.2.0")
	require.Zero(t, state.stopCalls)
	require.Zero(t, state.startCalls)
}

func TestManagedExternallyBoundsStartLockWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		home := setupKataEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("autostart = false\n"), 0600))
		ns, err := daemonruntime.NewNamespace()
		require.NoError(t, err)
		release, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).AcquireStartLock(t.Context())
		require.NoError(t, err)
		defer release()
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err = ensureLocalRunningTarget(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(started), time.Second)
	})
}

func TestManagedExternallyProfileWaitVerifiesIdentity(t *testing.T) {
	profile, root := localProfileFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n"), 0600))
	selection, err := InspectSelection(t.Context(), root, "")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ01"}`)
	}))
	defer server.Close()
	orig := discoverDaemonForAutoStart
	t.Cleanup(func() { discoverDaemonForAutoStart = orig })
	discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) {
		return ensureDiscovery{Outcome: daemonScanCompatible, Daemon: liveDaemon{BaseURL: server.URL, Record: kitdaemon.RuntimeRecord{Address: strings.TrimPrefix(server.URL, "http://"), Network: "tcp"}}}, nil
	}
	_, err = ensureLocalProfile(t.Context(), selection)
	require.ErrorIs(t, err, ErrProfileIdentityMismatch)
}

func TestManagedExternallyProfileWaitsDuringSchemaUpgrade(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		profile, root := localProfileFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(profile.Home, "config.toml"), []byte("autostart = false\n"), 0600))
		stageStorageSchema(t, filepath.Join(profile.Home, "kata.db"), db.CurrentSchemaVersion()-1)
		selection, err := InspectSelection(t.Context(), root, "")
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err = ensureLocalProfile(ctx, selection)
		require.ErrorIs(t, err, ErrDaemonManagedExternally)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}
