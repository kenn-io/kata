//go:build linux

package client

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestForeignOwnerHelperProcess(t *testing.T) {
	if os.Getenv("KATA_CLIENT_PIDNS_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	t.SkipNow()
}

// Auto-start must not spawn a daemon that can only wait for a store owned by
// a daemon in another PID namespace.
func TestAutoStartRefusesStoreOwnedFromAnotherPIDNamespace(t *testing.T) {
	setupKataEnv(t)
	child := exec.Command(os.Args[0], "-test.run=^TestForeignOwnerHelperProcess$") //nolint:gosec // test binary
	child.Env = append(os.Environ(), "KATA_CLIENT_PIDNS_HELPER=1")
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
	identity, ok := kitdaemon.ReadProcessIdentity(child.Process.Pid)
	require.True(t, ok)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: 1, ProcessIdentityV2: identity, StartedAt: time.Now().UTC(), Network: "tcp", Address: "127.0.0.1:1",
	})
	require.NoError(t, err)
	dsn, err := config.KataDSN(t.Context())
	require.NoError(t, err)
	release, err := daemonruntime.AcquireStoreOwnerLock(t.Context(), daemonruntime.StoreOwnerLockPaths(ns.DataDir, dsn))
	require.NoError(t, err)
	t.Cleanup(release)
	original := daemonruntime.ForeignStoreOwnerGrace
	t.Cleanup(func() { daemonruntime.ForeignStoreOwnerGrace = original })
	daemonruntime.ForeignStoreOwnerGrace = 100 * time.Millisecond
	spawns := 0
	originalStart := startDetachedDaemonForEnsure
	t.Cleanup(func() { startDetachedDaemonForEnsure = originalStart })
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error {
		spawns++
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err = autoStart(ctx, ns.DataDir)

	require.ErrorContains(t, err, "a kata daemon in another PID namespace owns this store")
	assert.Zero(t, spawns)
}
