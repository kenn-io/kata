package federation

import (
	"context"
	"time"

	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

// SetRelaySyncTurnBudgetForTest lowers the batches one relay sync turn moves
// per stream and the deliveries per batch, so a small project exceeds a turn.
func SetRelaySyncTurnBudgetForTest(batches, batchLimit int) func() {
	previousBatches, previousLimit := relaySyncTurnBatches, relaySyncBatchLimit
	relaySyncTurnBatches, relaySyncBatchLimit = batches, batchLimit
	return func() { relaySyncTurnBatches, relaySyncBatchLimit = previousBatches, previousLimit }
}

// SetEmbeddingProducerClockForTest replaces the clock that bounds how long a
// pending artifact offer defers local fill.
func SetEmbeddingProducerClockForTest(now func() time.Time) func() {
	previous := producerNow
	producerNow = now
	return func() { producerNow = previous }
}

// SyncFederationTurnForTest runs exactly one coordinated sync turn and reports
// whether it stopped at the turn budget with deliveries still pending.
func SyncFederationTurnForTest(ctx context.Context, store db.Storage, binding db.FederationBinding, creds config.FederationCredential) (bool, error) {
	finish, err := federationcoord.BeginSync(ctx, federationcoord.Key(store.InstanceUID(), binding.ProjectID), store, binding.ProjectID)
	if err != nil {
		return false, err
	}
	defer finish()
	return syncFederationOnceWithFence(ctx, store, binding, creds, clientOptsWithDefault(clientpkg.Opts{}), nil, nil)
}
