package pgstore

import (
	"io/fs"
)

// NewStoreForTesting builds a Store with the given dsn but no live *sql.DB.
// EXPORTED FOR TESTS ONLY — production callers cannot reach this. Used by
// Path() redaction tests that don't need to open a real connection.
func NewStoreForTesting(dsn string) *Store {
	return &Store{dsn: dsn}
}

// SetMigrationsSource swaps the migration FS the runner reads from. EXPORTED
// FOR TESTS ONLY — production callers cannot reach this. Returns a restore
// closure that callers run via t.Cleanup so a panicking test doesn't poison
// other tests' migration source.
func SetMigrationsSource(fsys fs.FS) func() {
	prev := migrationsSource
	migrationsSource = fsys
	return func() { migrationsSource = prev }
}

// EmbeddedMigrationsFS returns the production embedded FS so tests building
// synthetic ladders can anchor on the real baseline rather than re-staging the
// schema by hand.
func EmbeddedMigrationsFS() fs.FS {
	return migrationsFS
}
