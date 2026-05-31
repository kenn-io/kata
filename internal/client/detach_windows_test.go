//go:build windows

package client

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestDetachChildDetachesFromConsole pins the fix for the daemon's
// "git remote: exit status 0xc0000142" failure: the auto-started daemon must
// be created with DETACHED_PROCESS so it never inherits (and later outlives)
// the launching shell's console. Without DETACHED_PROCESS the daemon shares,
// then is orphaned from, a destroyed console and can no longer spawn git.
func TestDetachChildDetachesFromConsole(t *testing.T) {
	cmd := exec.Command("kata", "daemon", "start")
	detachChild(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("detachChild left SysProcAttr nil")
	}
	flags := cmd.SysProcAttr.CreationFlags
	if flags&detachedProcess == 0 {
		t.Errorf("DETACHED_PROCESS not set: CreationFlags = %#x", flags)
	}
	if flags&syscall.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Errorf("CREATE_NEW_PROCESS_GROUP not set: CreationFlags = %#x", flags)
	}
}

// TestDetachChildPreservesExistingFlags ensures detachChild ORs its flags in
// rather than clobbering any the caller already set.
func TestDetachChildPreservesExistingFlags(t *testing.T) {
	const sentinel = syscall.CREATE_UNICODE_ENVIRONMENT
	cmd := exec.Command("kata")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: sentinel}
	detachChild(cmd)

	if cmd.SysProcAttr.CreationFlags&sentinel == 0 {
		t.Errorf("detachChild dropped pre-existing flag: %#x", cmd.SysProcAttr.CreationFlags)
	}
}
