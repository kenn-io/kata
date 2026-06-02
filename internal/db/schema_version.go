package db

// currentSchemaVersion is the schema_version this binary expects to find in
// meta. Backends apply the canonical schema.sql to fresh databases and
// compare on-disk state against this number when opening existing ones; a
// mismatch surfaces either ErrSchemaCutoverRequired (for older sources, so
// storeopen can drive a JSONL cutover) or a newer-than-binary error.
const currentSchemaVersion = 12

// CurrentSchemaVersion returns the schema version expected by this binary.
// Backends and the cutover path read this to align freshly created databases
// with the source's schema row.
func CurrentSchemaVersion() int { return currentSchemaVersion }
