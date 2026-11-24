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

// SetRefusalClockForTest replaces the clock that schedules this runner's relay
// refusal retries.
func (r *Runner) SetRefusalClockForTest(now func() time.Time) { r.refusalClock = now }

// SetDebounceTimerForTest replaces the timer this runner starts when a wake
// reaches it while it waits for its interval.
func (r *Runner) SetDebounceTimerForTest(newTimer func(time.Duration) Timer) {
	r.debounceTimer = newTimer
}
