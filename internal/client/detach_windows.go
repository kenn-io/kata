//go:build windows

package client

import (
	"os/exec"
	"syscall"
)

// DETACHED_PROCESS is not exported by the syscall package; its value is
// fixed in winbase.h. A process created with this flag has no console.
const detachedProcess = 0x00000008

// detachChild fully decouples the auto-started daemon from the launching
// shell's console.
//
// On Windows a child created without special flags shares its parent's
// console. When that console is later torn down — the foreground `kata`
// process that auto-started the daemon exits — the long-lived daemon is left
// holding a handle to a destroyed console. Any console subprocess it then
// spawns fails at process startup: Git for Windows in particular is built on
// the msys2 runtime, which requires a usable console to initialize, so it
// aborts with STATUS_DLL_INIT_FAILED (reported by Go as "exit status
// 0xc0000142"). The daemon resolves project identity by running `git remote`,
// so every command that reaches the daemon then fails.
//
// DETACHED_PROCESS gives the daemon no console at all, so it never depends on
// the launcher's console lifetime. When the daemon later spawns git, Windows
// allocates a fresh console for that child and git initializes normally. This
// mirrors the Unix detachChild, which uses Setpgid to isolate the daemon from
// the foreground process group. CREATE_NEW_PROCESS_GROUP additionally ensures
// a Ctrl+C / Ctrl+Break aimed at the caller is never delivered to the daemon.
func detachChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP
}
