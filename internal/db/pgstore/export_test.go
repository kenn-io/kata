package pgstore

import (
	"context"
	"testing"
	"time"
)

const BatchProjectStatsSQLForTest = batchProjectStatsSQL

// NewStoreForTesting builds a Store with the given dsn but no live *sql.DB.
// EXPORTED FOR TESTS ONLY — production callers cannot reach this. Used by
// Path() redaction tests that don't need to open a real connection.
func NewStoreForTesting(dsn string) *Store {
	return &Store{dsn: dsn}
}

// InstallEnrollmentRotationStageForTest observes the transaction after its
// replacement lookup without adding a production Store method.
func InstallEnrollmentRotationStageForTest(
	store *Store,
	stage func(context.Context) error,
) func() {
	previous := store.rotationStage
	store.rotationStage = stage
	return func() {
		store.rotationStage = previous
	}
}

// InstallExternalRootClockForTest freezes the binding transaction clock and
// returns a restore function for deterministic millisecond-boundary tests.
func InstallExternalRootClockForTest(store *Store, now func() time.Time) func() {
	previous := store.externalRootNow
	store.externalRootNow = now
	return func() { store.externalRootNow = previous }
}

func InstallRelayHistoryFullScanObserverForTest(store *Store, observe func()) func() {
	previous := store.relayHistoryFullScanObserver
	store.relayHistoryFullScanObserver = observe
	return func() { store.relayHistoryFullScanObserver = previous }
}

const EventSelectForTest = eventSelect

var ScanEventForTest = scanEvent

// SetStatementTimeoutForTest shortens the per-statement limit for connections
// opened after the call. A test that calls it must not run in parallel.
func SetStatementTimeoutForTest(t testing.TB, timeout string) {
	t.Helper()
	old := statementTimeout
	statementTimeout = timeout
	t.Cleanup(func() { statementTimeout = old })
}
