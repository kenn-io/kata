package db

import (
	"database/sql"
	"fmt"
	"time"
)

// StrongerArtifactRetention picks the retention for two stored copies of the
// same artifact bytes. Durable (no expiry) wins over staging; between two
// staged copies the chronologically later expiry wins. Stored expiries are
// RFC3339Nano text, whose trimmed fractions do not sort as time.
func StrongerArtifactRetention(existing, incoming sql.NullString) (sql.NullString, error) {
	if !existing.Valid || !incoming.Valid {
		return sql.NullString{}, nil
	}
	existingAt, err := time.Parse(time.RFC3339Nano, existing.String)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("parse stored artifact staging expiry %q: %w", existing.String, err)
	}
	incomingAt, err := time.Parse(time.RFC3339Nano, incoming.String)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("parse restored artifact staging expiry %q: %w", incoming.String, err)
	}
	if incomingAt.After(existingAt) {
		return incoming, nil
	}
	return existing, nil
}
