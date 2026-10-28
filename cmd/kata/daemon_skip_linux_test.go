//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

// startForeignNamespaceProcess starts a helper in new user and PID namespaces
// and returns its host PID; it runs until the test ends.
func startForeignNamespaceProcess(t *testing.T) int {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestForeignNamespaceHelperProcess$") //nolint:gosec // test binary
	child.Env = append(os.Environ(), "KATA_PIDNS_HELPER=1")
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	if err := child.Start(); err != nil {
		_ = stdin.Close()
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) {
			t.Skipf("user/PID namespace unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = child.Wait() })
	return child.Process.Pid
}

func TestForeignNamespaceHelperProcess(t *testing.T) {
	if os.Getenv("KATA_PIDNS_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	t.SkipNow()
}

// writeForeignRuntimeRecord writes a runtime record for a live daemon in
// another PID namespace, which records its PID there as 1.
func writeForeignRuntimeRecord(t *testing.T) {
	t.Helper()
	identity, ok := kitdaemon.ReadProcessIdentity(startForeignNamespaceProcess(t))
	require.True(t, ok)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: 1, ProcessIdentityV2: identity, StartedAt: time.Now().UTC(),
		Network: "tcp", Address: closedLoopbackAddr(t),
	})
	require.NoError(t, err)
}

// A record written in another PID namespace names its PID there, so stop must
// skip it and say why.
func TestDaemonStopNamesForeignNamespaceSkip(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	writeForeignRuntimeRecord(t)

	_, stderr, err := executeRootCapture(t, t.Context(), "--agent", "daemon", "stop")

	require.NoError(t, err)
	assert.Contains(t, stderr, "skipping daemon pid=1 (another PID namespace)\n")
}

// A daemon in another PID namespace keeps the store until it stops; it is not
// a restarting local daemon, so startup refuses after a short grace instead of
// waiting out the full owner-lock wait.
func TestStartRefusesStoreOwnedFromAnotherPIDNamespace(t *testing.T) {
	for _, entry := range []string{"foreground", "detached"} {
		t.Run(entry, func(t *testing.T) {
			resetFlags(t)
			home := setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			t.Setenv("PORT", "")
			writeForeignRuntimeRecord(t)
			holdStoreOwnerLock(t, home, filepath.Join(home, "kata.db"))
			original := daemonruntime.ForeignStoreOwnerGrace
			t.Cleanup(func() { daemonruntime.ForeignStoreOwnerGrace = original })
			daemonruntime.ForeignStoreOwnerGrace = 100 * time.Millisecond
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)

			if entry == "foreground" {
				err = runDaemon(t.Context())
			} else {
				_, err = defaultLaunchDetachedDaemon(t.Context(), ns.DataDir, "", false, true)
			}

			require.ErrorContains(t, err, "a kata daemon in another PID namespace owns this store")
			require.NoFileExists(t, filepath.Join(home, "kata.db"), "must refuse before opening storage")
		})
	}
}
