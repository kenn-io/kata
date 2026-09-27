package daemon

import (
	"testing"
	"time"
)

// SetShowClaimStatusRefreshTimeoutForTest shortens the hub refresh budget; call it before testenv.New so the restore runs after the daemon stops.
func SetShowClaimStatusRefreshTimeoutForTest(t testing.TB, timeout time.Duration) {
	t.Helper()
	previous := showClaimStatusRefreshTimeout
	showClaimStatusRefreshTimeout = timeout
	t.Cleanup(func() { showClaimStatusRefreshTimeout = previous })
}
