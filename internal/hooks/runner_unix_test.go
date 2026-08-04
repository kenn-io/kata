//go:build !windows

package hooks

import (
	"testing"
	"time"
)

// TestRunner_KillTree_OrphanedChildrenDieToo pins that timeout cleanup
// escalates to SIGKILL on the entire process group. The hook's parent
// (hookprobe spawn-orphan) ignores SIGTERM AND outlives the hook
// timeout, so the runner cannot fall back to "process exited normally"
// — the only way the run can finish is for the SIGTERM/grace/SIGKILL
// path to take down the whole group. The test asserts a timed_out
// result and that the run completes well before the 30s child sleep
// expires.
func TestRunner_KillTree_OrphanedChildrenDieToo(t *testing.T) {
	rs := newRunnerSetup(t)
	rs.deps.GraceWindow = 100 * time.Millisecond
	start := time.Now()
	got := rs.runProbe(func(h *ResolvedHook) {
		h.Index = 11
		h.Args = []string{"spawn-orphan", "30s"}
		h.Timeout = 100 * time.Millisecond
	})
	if got.Result != "timed_out" {
		t.Fatalf("result = %q, want timed_out (rs log: %s)", got.Result, rs.logBuf.String())
	}
	// Bound: full kill sequence (SIGTERM + 100ms grace + SIGKILL +
	// reap) must finish well before the 30s child sleep. If the group
	// SIGKILL regressed, this would block until the 30s child exits.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runJob took %s, suggests orphan not killed", elapsed)
	}
}
