package pgstore

import "context"

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
