package db

import (
	"context"
	"strings"
)

// FormerInstanceUIDMetaPrefix stores identities retained by independent restores.
const FormerInstanceUIDMetaPrefix = "former_instance_uid."

// LocalInstanceOriginSQL matches the current instance and identities retained by restores.
// column and current are SQL expressions supplied by the caller.
func LocalInstanceOriginSQL(column, current string) string {
	return "(" + column + "=" + current + " OR EXISTS(SELECT 1 FROM meta WHERE key='" + FormerInstanceUIDMetaPrefix + "' || " + column + "))"
}

// ReadFormerInstanceUIDs reads the former identities from installation metadata.
func ReadFormerInstanceUIDs(ctx context.Context, q SpokeNameReader) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT key FROM meta WHERE substr(key,1,$1)=$2 ORDER BY key`, len(FormerInstanceUIDMetaPrefix), FormerInstanceUIDMetaPrefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		result = append(result, strings.TrimPrefix(key, FormerInstanceUIDMetaPrefix))
	}
	return result, rows.Err()
}
