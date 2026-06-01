package sqlitestore

import "io/fs"

// SetMigrationsSource swaps the migration FS the runner reads from. Tests in
// the external sqlitestore_test package call this to inject synthetic
// migration ladders without touching the embedded production ladder. The
// returned func restores the previous source; pass it to t.Cleanup so the
// override never leaks between tests.
func SetMigrationsSource(fsys fs.FS) func() {
	prev := migrationsSource
	migrationsSource = fsys
	return func() { migrationsSource = prev }
}

// EmbeddedMigrationsFS exposes the production embedded FS so tests can anchor
// synthetic ladders on the real baseline.
func EmbeddedMigrationsFS() fs.FS {
	return migrationsFS
}
