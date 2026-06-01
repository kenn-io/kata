package db

// currentSchemaVersion is the schema_version this binary expects to find in
// meta. Backends compare on-disk state against this number to decide whether
// to run, advance the schema through the migration runner, or refuse with
// ErrSchemaOutOfDate.
//
// Phase 3 bumped this from 12 to 13 to land the idempotency UNIQUE partial
// index on both backends. BaselineSchemaVersion stays pinned at 12 — it is
// the floor of the embedded migration ladder, not a tracking alias.
const currentSchemaVersion = 13

// CurrentSchemaVersion returns the schema version expected by this binary.
// Backends and the cutover path read this to align freshly created databases
// with the source's schema row.
func CurrentSchemaVersion() int { return currentSchemaVersion }
