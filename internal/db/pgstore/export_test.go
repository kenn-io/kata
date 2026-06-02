package pgstore

// NewStoreForTesting builds a Store with the given dsn but no live *sql.DB.
// EXPORTED FOR TESTS ONLY — production callers cannot reach this. Used by
// Path() redaction tests that don't need to open a real connection.
func NewStoreForTesting(dsn string) *Store {
	return &Store{dsn: dsn}
}
