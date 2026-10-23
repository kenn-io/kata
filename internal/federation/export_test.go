package federation

import "time"

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
