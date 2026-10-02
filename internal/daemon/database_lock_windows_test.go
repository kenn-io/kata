package daemon

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Fresh paths beneath Windows directory junctions must share the target's lock,
// just as fresh paths beneath ordinary directory symlinks do on Unix.
func TestDatabaseLockFreshPathBelowJunction(t *testing.T) {
	target := t.TempDir()
	junction := filepath.Join(t.TempDir(), "junction")
	out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput() //nolint:gosec // G204: mklink uses the test's temporary junction and target paths.
	if err != nil {
		t.Skipf("junction creation unavailable: %s: %v", out, err)
	}
	release, err := AcquireDatabaseLock(filepath.Join(junction, "new.db"))
	require.NoError(t, err)
	defer release()
	second, err := AcquireDatabaseLock(filepath.Join(target, "new.db"))
	if second != nil {
		second()
	}
	require.ErrorContains(t, err, "daemon already running")
}
