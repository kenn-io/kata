package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
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

// SelectLocalProducerForTest runs producer selection for one project with the
// given binding and recipe, delivering project events to onEvent.
func SelectLocalProducerForTest(ctx context.Context, store db.Storage, project db.Project, binding db.FederationBinding, recipe embedding.ArtifactIdentity, onEvent func(db.Event)) (bool, error) {
	r := NewReconciler(store, nil, nil, ReconcilerConfig{OnProjectEvent: func(e db.Event, _ activity.Admission) { onEvent(e) }})
	return r.selectLocalProducer(ctx, project, binding, recipe)
}

// ProjectGenerationAllowedForTest runs the generation gate for one project.
func ProjectGenerationAllowedForTest(ctx context.Context, store db.Storage, projectUID string, recipe embedding.ArtifactIdentity) (bool, error) {
	return NewReconciler(store, nil, nil, ReconcilerConfig{}).projectGenerationAllowed(ctx, projectUID, recipe, false)
}

// ReconcileOnceForTest runs one reconcile pass with default batching.
func ReconcileOnceForTest(ctx context.Context, store db.Storage, idx *vector.Index, emb interface {
	EncodeFunc() kitvec.EncodeFunc
	Generation() kitvec.Generation
}) error {
	return NewReconciler(store, idx, emb, ReconcilerConfig{BatchSize: 64}).reconcileOnce(ctx)
}
