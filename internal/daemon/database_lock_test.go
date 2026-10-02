package daemon

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/version"
)

// The lock identity is the database, independent of build version, home,
// listener, and symlink spelling. Process exit must release ownership.
func TestDatabaseLockAcrossVersionsAndProcessExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kata.db")
	require.NoError(t, os.WriteFile(path, nil, 0600))
	child := exec.Command(os.Args[0], "-test.run=^TestDatabaseLockChild$") //nolint:gosec // G204: re-execute the current test binary with a fixed test selector.
	child.Env = append(os.Environ(), "KATA_TEST_LOCK_PATH="+path)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	release, err := AcquireDatabaseLock(path)
	if release != nil {
		release()
	}
	require.ErrorContains(t, err, "daemon already running")
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(path, alias); err == nil {
		release, err = AcquireDatabaseLock(alias)
		if release != nil {
			release()
		}
		require.ErrorContains(t, err, "daemon already running")
	}
	require.NoError(t, stdin.Close())
	require.NoError(t, child.Wait())
	release, err = AcquireDatabaseLock(path)
	require.NoError(t, err)
	release()
	release, err = AcquireDatabaseLock(path)
	require.NoError(t, err)
	release()
}

func TestDatabaseLockChild(t *testing.T) {
	path := os.Getenv("KATA_TEST_LOCK_PATH")
	if path == "" {
		return
	}
	version.Version = "v0.18.0"
	release, err := AcquireDatabaseLock(path)
	require.NoError(t, err)
	defer release()
	_, _ = os.Stdout.WriteString("ready\n")
	_, _ = os.Stdin.Read(make([]byte, 1))
}

func TestDatabaseLockBeforeFreshDatabaseExists(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "new-home", "kata.db")
	release, err := AcquireDatabaseLock(path)
	require.NoError(t, err)
	defer release()
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "taking ownership must not initialize SQLite")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Dir(path), alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	second, err := AcquireDatabaseLock(filepath.Join(alias, "kata.db"))
	if second != nil {
		second()
	}
	require.ErrorContains(t, err, "daemon already running")
}
