package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaHasEmbeddingSurface(t *testing.T) {
	d := openTestDB(t)

	// issues.content_revision exists and is NOT NULL.
	assertColumn(t, d, "issues", "content_revision", "INTEGER", true)

	// issue_embeddings table exists with its six expected columns.
	assertSchemaObject(t, d, "issue_embeddings")
	var n int
	require.NoError(t, d.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM pragma_table_info('issue_embeddings')
		   WHERE name IN ('issue_id','embedded_content_revision','embed_fingerprint',
		                  'dims','vector_bytes','updated_at')`,
	).Scan(&n))
	assert.Equal(t, 6, n, "issue_embeddings must expose all six expected columns")
}
