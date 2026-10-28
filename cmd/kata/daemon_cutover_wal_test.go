package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/rootbridge"
	"go.kenn.io/kata/internal/telemetry"
)

func TestForegroundRefusesStartupBeforeCutover(t *testing.T) {
	for _, tc := range []struct {
		name             string
		occupiedEndpoint bool
		ownedStore       bool
		ownerAlias       bool
		ownerDBLink      bool
		wantError        string
	}{
		{name: "live local record", wantError: "daemon already running"},
		{name: "occupied endpoint", occupiedEndpoint: true, wantError: "bind"},
		// A daemon in another PID namespace keeps a record this process cannot
		// judge and a socket under its own runtime dir; only the store lock is shared.
		{name: "store owned without a blocking record", ownedStore: true, wantError: "another kata daemon owns this store"},
		// A container bind mount or sandbox can reach the same database through
		// another path, which hashes to a different runtime namespace.
		{name: "store owned through another path", ownedStore: true, ownerAlias: true, wantError: "another kata daemon owns this store"},
		{name: "store owned through a database file symlink", ownedStore: true, ownerDBLink: true, wantError: "another kata daemon owns this store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			t.Setenv("PORT", "")
			path := filepath.Join(home, "kata.db")
			source, err := sqlitestore.Open(t.Context(), path)
			require.NoError(t, err)
			_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
			require.NoError(t, err)
			require.NoError(t, source.Close())
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			switch {
			case tc.occupiedEndpoint:
				occupied, listenErr := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, listenErr)
				t.Cleanup(func() { require.NoError(t, occupied.Close()) })
				err = runDaemonWithListen(ctx, occupied.Addr().String(), false, false)
			case tc.ownedStore:
				ownerHome, ownerDB := home, path
				switch {
				case tc.ownerAlias:
					ownerHome = filepath.Join(t.TempDir(), "alias")
					ownerDB = filepath.Join(ownerHome, "kata.db")
					if err := os.Symlink(home, ownerHome); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				case tc.ownerDBLink:
					ownerDB = filepath.Join(t.TempDir(), "linked.db")
					if err := os.Symlink(path, ownerDB); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				holdStoreOwnerLock(t, ownerHome, ownerDB)
				original := daemonOwnerLockWait
				t.Cleanup(func() { daemonOwnerLockWait = original })
				daemonOwnerLockWait = 200 * time.Millisecond
				err = runDaemon(ctx)
			default:
				writeRuntimePID(t, home, startSleepProcess(t).Process.Pid)
				err = runDaemon(ctx)
			}
			require.ErrorContains(t, err, tc.wantError)
			version, err := sqlitestore.PeekSchemaVersion(t.Context(), path)
			require.NoError(t, err)
			require.Equal(t, 28, version, "must refuse before migrating storage")
		})
	}
}

// holdStoreOwnerLock takes the store owner lock in a child process using
// home and dbPath, as a daemon in another PID namespace or mount would.
func holdStoreOwnerLock(t *testing.T, home, dbPath string) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestStoreOwnerLockHelperProcess$") //nolint:gosec // test helper starts this test binary
	child.Env = append(os.Environ(), "KATA_OWNER_LOCK_HELPER=1", "KATA_HOME="+home, "KATA_DB="+dbPath)
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = child.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err, "helper did not take the owner lock")
	require.Equal(t, "owner-locked\n", line)
}

func TestStoreOwnerLockHelperProcess(t *testing.T) {
	if os.Getenv("KATA_OWNER_LOCK_HELPER") != "1" {
		return
	}
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	dsn, err := config.KataDSN(t.Context())
	require.NoError(t, err)
	release, err := daemonruntime.AcquireStoreOwnerLock(t.Context(), daemonruntime.StoreOwnerLockPaths(ns.DataDir, dsn))
	require.NoError(t, err)
	defer release()
	_, err = os.Stdout.WriteString("owner-locked\n")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// A daemon that upgraded its store through a symlink keeps owning it, so a
// competing daemon configured with the same link is refused.
func TestStoreStaysOwnedAfterCutoverThroughSymlink(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	t.Setenv("PORT", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	originalTelemetry := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client { return &fakeTelemetryReporter{} }
	t.Cleanup(func() { newTelemetryReporter = originalTelemetry })
	target := filepath.Join(home, "kata.db")
	source, err := sqlitestore.Open(t.Context(), target)
	require.NoError(t, err)
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	link := filepath.Join(t.TempDir(), "linked.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("KATA_DB", link)
	// The external-root runner starts after storage opens, so cutover is done.
	originalRun := runExternalRootRunner
	started := make(chan struct{}, 1)
	runExternalRootRunner = func(ctx context.Context, _ *rootbridge.Runner) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { runExternalRootRunner = originalRun })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		// Closing lets both the exit check and cleanup observe the exit.
		defer close(done)
		done <- runDaemonWithListen(ctx, "127.0.0.1:0", false, false)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("daemon exited before serving: %v", err)
	}
	original := daemonOwnerLockWait
	t.Cleanup(func() { daemonOwnerLockWait = original })
	daemonOwnerLockWait = 200 * time.Millisecond
	competitorCtx, competitorCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer competitorCancel()
	err = runDaemonWithListen(competitorCtx, "127.0.0.1:0", false, false)
	require.ErrorContains(t, err, "another kata daemon owns this store")
}

// SQLite creates a missing database through a dangling symlink, so an owner
// configured through that link must also own the target's name.
func TestForegroundRefusesStoreOwnedThroughDanglingSymlink(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	t.Setenv("PORT", "")
	target := filepath.Join(home, "fresh.db")
	link := filepath.Join(t.TempDir(), "linked.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	holdStoreOwnerLock(t, home, link)
	original := daemonOwnerLockWait
	t.Cleanup(func() { daemonOwnerLockWait = original })
	daemonOwnerLockWait = 200 * time.Millisecond
	t.Setenv("KATA_DB", target)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	require.ErrorContains(t, runDaemon(ctx), "another kata daemon owns this store")
	_, err := os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist, "must refuse before creating the database")
}
