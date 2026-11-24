package db

import (
	"context"
	"strings"
)

// FormerInstanceUIDMetaPrefix stores identities retained by independent restores.
const FormerInstanceUIDMetaPrefix = "former_instance_uid."

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
