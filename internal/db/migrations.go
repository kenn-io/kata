package db

// BaselineSchemaVersion is the floor of the embedded migration ladder.
// Databases at this version or higher advance through Storage.Migrate;
// databases below it must first be brought up via internal/jsonl.AutoCutover,
// which exports the legacy shape and re-imports it into a fresh
// baseline-shaped DB.
//
// Pinned at 12: the literal value the JSONL-cutover boundary lives at. Future
// migrations advance currentSchemaVersion only and leave BaselineSchemaVersion
// fixed. Phase 2 shipped the baseline; Phase 3 added an idempotency UNIQUE
// migration on top, so currentSchemaVersion = 13 while baseline stays at 12.
const BaselineSchemaVersion = 12

// MigrationResult describes what a Storage.Migrate call did. The zero value
// represents "no migration ran" — either because the caller did not pass
// db.ApplyMigrations() to storeopen.Open or because the database was already
// at the current schema version.
type MigrationResult struct {
	// From is the meta.schema_version observed before the run. Zero for a
	// fresh database (no meta table).
	From int
	// To is the meta.schema_version recorded after the run completes. When
	// the migration succeeds this equals db.CurrentSchemaVersion(). When the
	// run was a no-op it equals From.
	To int
	// Applied is the ordered list of versions advanced through, in ascending
	// order. nil when no work was done (Migrate was a no-op).
	Applied []int
}
