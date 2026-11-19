package sqlitestore

import (
	"context"
	"database/sql/driver"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv/sqlcount"
)

// LinksChangedPayloadForTest exposes linksChangedPayload to external tests so
// they can pin the exact wire bytes without making the function public.
var LinksChangedPayloadForTest = linksChangedPayload

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

// MaterializeFederatedProjectOnceForTest runs one materialization attempt
// without RetryTransient, so tests can observe a lost writer race directly.
func MaterializeFederatedProjectOnceForTest(ctx context.Context, store *Store, projectID int64) error {
	return store.materializeFederatedProject(ctx, projectID)
}

const EventSelectForTest = eventSelect

var ScanEventForTest = scanEvent

// CountStatementsForTest wraps the SQLite driver used by stores opened after
// this call, so they count statements run under a sqlcount.Counter context.
func CountStatementsForTest() func() {
	previous := registeredSQLiteDriver
	registeredSQLiteDriver = func() (driver.Driver, error) {
		inner, err := previous()
		if err != nil {
			return nil, err
		}
		return sqlcount.WrapDriver(inner), nil
	}
	return func() { registeredSQLiteDriver = previous }
}

// ForgetFederationFeedForTest clears what store remembers about earlier
// federation polls.
func ForgetFederationFeedForTest(store *Store) {
	store.federationFeed = &db.FederationFeedCache{}
}
