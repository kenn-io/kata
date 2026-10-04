package sqlitelock

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
	child.Env = []string{
		"PATH=" + os.Getenv("PATH"), "SystemRoot=" + os.Getenv("SystemRoot"),
		"KATA_HOME=" + t.TempDir(), "KATA_DB=" + path, "KATA_TEST_LOCK_PATH=" + path,
	}
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	lock, err := Acquire(path)
	if lock != nil {
		lock.Release()
	}
	require.ErrorContains(t, err, "daemon already running")
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(path, alias); err == nil {
		lock, err = Acquire(alias)
		if lock != nil {
			lock.Release()
		}
		require.ErrorContains(t, err, "daemon already running")
	}
	require.NoError(t, stdin.Close())
	require.NoError(t, child.Wait())
	lock, err = Acquire(path)
	require.NoError(t, err)
	lock.Release()
	lock, err = Acquire(path)
	require.NoError(t, err)
	lock.Release()
}

func TestDatabaseLockChild(t *testing.T) {
	path := os.Getenv("KATA_TEST_LOCK_PATH")
	if path == "" {
		return
	}
	version.Version = "v0.18.0"
	lock, err := Acquire(path)
	require.NoError(t, err)
	defer lock.Release()
	_, _ = os.Stdout.WriteString("ready\n")
	_, _ = os.Stdin.Read(make([]byte, 1))
}

func TestDatabaseLockBeforeFreshDatabaseExists(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "new-home", "kata.db")
	lock, err := Acquire(path)
	require.NoError(t, err)
	defer lock.Release()
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "taking ownership must not initialize SQLite")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Dir(path), alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	second, err := Acquire(filepath.Join(alias, "kata.db"))
	if second != nil {
		second.Release()
	}
	require.ErrorContains(t, err, "daemon already running")
}
