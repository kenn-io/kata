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
)

func TestForegroundRefusesStartupBeforeCutover(t *testing.T) {
	for _, tc := range []struct {
		name             string
		occupiedEndpoint bool
		ownedStore       bool
		ownerAlias       bool
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
				ownerHome := home
				if tc.ownerAlias {
					ownerHome = filepath.Join(t.TempDir(), "alias")
					if err := os.Symlink(home, ownerHome); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				holdStoreOwnerLock(t, ownerHome)
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
// home as KATA_HOME, as a daemon in another PID namespace or mount would.
func holdStoreOwnerLock(t *testing.T, home string) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestStoreOwnerLockHelperProcess$") //nolint:gosec // test helper starts this test binary
	child.Env = append(os.Environ(), "KATA_OWNER_LOCK_HELPER=1", "KATA_HOME="+home, "KATA_DB="+filepath.Join(home, "kata.db"))
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
	release, err := daemonruntime.AcquireStoreOwnerLock(t.Context(), daemonruntime.StoreOwnerLockPath(ns.DataDir, dsn))
	require.NoError(t, err)
	defer release()
	_, err = os.Stdout.WriteString("owner-locked\n")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, os.Stdin)
}
