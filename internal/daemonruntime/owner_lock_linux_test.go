//go:build linux

package daemonruntime_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestOwnerLockForeignHelperProcess(t *testing.T) {
	if os.Getenv("KATA_OWNER_PIDNS_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	t.SkipNow()
}

// The daemon in another PID namespace can exit during the grace while a local
// starter takes the store lock before the next poll. The lock never looks
// free, but the refusal must not blame a daemon whose record is gone.
func TestRefuseForeignStoreOwnerRechecksRecordBeforeRefusing(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestOwnerLockForeignHelperProcess$") //nolint:gosec // test binary
	child.Env = append(os.Environ(), "KATA_OWNER_PIDNS_HELPER=1")
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
	dataDir := t.TempDir()
	record, err := (kitdaemon.RuntimeStore{Dir: dataDir}).Write(kitdaemon.RuntimeRecord{
		PID: 1, ProcessIdentityV2: identity, StartedAt: time.Now().UTC(), Network: "tcp", Address: "127.0.0.1:1",
	})
	require.NoError(t, err)
	paths := []string{filepath.Join(dataDir, "kata.db.owner.lock")}
	release, err := daemonruntime.AcquireStoreOwnerLock(t.Context(), paths)
	require.NoError(t, err)
	defer release()

	synctest.Test(t, func(t *testing.T) {
		go func() {
			time.Sleep(daemonruntime.ForeignStoreOwnerGrace / 3)
			_ = os.Remove(record)
		}()

		require.NoError(t, daemonruntime.RefuseForeignStoreOwner(t.Context(), dataDir, paths))
	})
}
