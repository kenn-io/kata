package daemon

import (
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/db"
)

// SetShowClaimStatusRefreshTimeoutForTest shortens the hub refresh budget; call it before testenv.New so the restore runs after the daemon stops.
func SetShowClaimStatusRefreshTimeoutForTest(t testing.TB, timeout time.Duration) {
	t.Helper()
	previous := showClaimStatusRefreshTimeout
	showClaimStatusRefreshTimeout = timeout
	t.Cleanup(func() { showClaimStatusRefreshTimeout = previous })
}

// ForgetFederationReplicaTransitionsForTest drops every in-memory leave state
// held for store's daemon, as a daemon restart does. Durable credential markers
// are untouched.
func ForgetFederationReplicaTransitionsForTest(store db.Storage) {
	prefix := store.InstanceUID() + "\x00"
	federationReplicaTransitions.mu.Lock()
	defer federationReplicaTransitions.mu.Unlock()
	for key := range federationReplicaTransitions.transitions {
		if strings.HasPrefix(key, prefix) {
			delete(federationReplicaTransitions.transitions, key)
		}
	}
}

// FederationReplicaLeavePreparedForTest reports whether any leave preparation
// for projectName is outstanding.
func FederationReplicaLeavePreparedForTest(store db.Storage, projectName string) bool {
	federationReplicaTransitions.mu.RLock()
	defer federationReplicaTransitions.mu.RUnlock()
	transition, ok := federationReplicaTransitions.transitions[federationReplicaTransitionKey(store, projectName)]
	return ok && len(transition.preparations) > 0
}
