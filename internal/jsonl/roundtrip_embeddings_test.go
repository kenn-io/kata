package jsonl_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// TestImportSkipsLegacyIssueEmbeddingRecords proves that pre-v23 archives
// carrying issue_embedding envelopes (the legacy issue_embeddings table,
// dropped in Task 11) still import cleanly. Vectors are now derived state
// rebuilt by the embedding reconciler from the vector sidecar, so the
// legacy envelope is acknowledged and dropped rather than erroring old
// archives or requiring the now-removed issue_embeddings table.
func TestImportSkipsLegacyIssueEmbeddingRecords(t *testing.T) {
	ctx := context.Background()
	const (
		projUID  = "01HZZZZZZZZZZZZZZZZZZZZZZZ"
		issueUID = "01HZZZZZZZZZZZZZZZZZZZZA01"
	)
	vectorB64 := base64.StdEncoding.EncodeToString(make([]byte, 8)) // dims=2 float32s

	rows := []string{
		`{"kind":"meta","data":{"key":"export_version","value":"22"}}`,
		`{"kind":"project","data":{"id":1,"uid":"` + projUID + `","name":"legacy-embed-project","metadata":{},"revision":1,"created_at":"2026-01-01T00:00:00.000Z"}}`,
		`{"kind":"issue","data":{"id":1,"uid":"` + issueUID + `","project_id":1,"short_id":"za01","title":"legacy embed issue","body":"","status":"open","closed_reason":null,"owner":null,"author":"tester","created_at":"2026-01-01T00:00:01.000Z","updated_at":"2026-01-01T00:00:01.000Z","closed_at":null,"deleted_at":null,"metadata":{},"revision":1}}`,
		`{"kind":"issue_embedding","data":{"issue_uid":"` + issueUID + `","embedded_content_revision":0,"embed_fingerprint":"` + strings.Repeat("a", 64) + `","dims":2,"vector_b64":"` + vectorB64 + `"}}`,
	}

	// Import the same archive into two fresh targets: the legacy embedding
	// record must not error and must not prevent the issue from landing,
	// and the outcome must be identical either time (idempotent import).
	first := openImportTargetDB(t)
	require.NoError(t, importJSONL(ctx, first, rows...))
	second := openImportTargetDB(t)
	require.NoError(t, importJSONL(ctx, second, rows...))

	firstIssue, err := first.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "legacy embed issue", firstIssue.Title)

	secondIssue, err := second.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "legacy embed issue", secondIssue.Title)
}
