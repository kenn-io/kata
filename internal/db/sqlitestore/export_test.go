package sqlitestore

import (
	"context"
	"time"
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
