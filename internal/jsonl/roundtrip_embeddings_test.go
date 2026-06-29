package jsonl_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

// repeat63jsonl is a 63-char zero run; prefixing a leading char yields a
// 64-char fingerprint that satisfies the schema CHECK.
const repeat63jsonl = "000000000000000000000000000000000000000000000000000000000000000"

// TestEmbeddingsRoundTripPreservesContentRevision proves the JSONL export
// carries issues.content_revision and the issue_embeddings rows, so an issue
// that was edited (content_revision > 0) then embedded is not falsely stale
// after a cutover/backup round-trip: ListEmbedTargets must return nothing for
// it under the same fingerprint.
func TestEmbeddingsRoundTripPreservesContentRevision(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	proj, err := src.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	iss, _, err := src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "t", Body: "b", Author: "a",
	})
	require.NoError(t, err)
	nt := "edited"
	_, _, _, err = src.EditIssue(ctx, db.EditIssueParams{IssueID: iss.ID, Title: &nt, Actor: "a"})
	require.NoError(t, err)

	var cr int64
	require.NoError(t, src.QueryRowContext(ctx,
		`SELECT content_revision FROM issues WHERE id=?`, iss.ID).Scan(&cr))
	require.Greater(t, cr, int64(0), "edit must bump content_revision above zero")

	fp := "a" + repeat63jsonl
	require.NoError(t, src.UpsertIssueEmbedding(ctx, db.IssueEmbedding{
		IssueID: iss.ID, EmbeddedContentRevision: cr, Fingerprint: fp, Dims: 2, Vector: []float32{1, 0},
	}))

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{}))

	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dst))

	// Imported issue carries content_revision, so the imported embedding is
	// NOT stale: ListEmbedTargets returns nothing under the same fingerprint.
	targets, err := dst.ListEmbedTargets(ctx, fp, 10)
	require.NoError(t, err)
	require.Empty(t, targets, "imported embedding falsely stale")

	// And the vector survived the round-trip intact.
	var dims int
	var raw []byte
	require.NoError(t, dst.QueryRowContext(ctx, `
		SELECT e.dims, e.vector_bytes
		  FROM issue_embeddings e
		  JOIN issues i ON i.id = e.issue_id
		 WHERE i.uid = ?`, iss.UID).Scan(&dims, &raw))
	require.Equal(t, 2, dims)
	require.Len(t, raw, 8, "2 float32 == 8 bytes")
}

// TestEmbeddingImportDropsRowExceedingContentRevision proves the import
// validation: an embedding whose embedded_content_revision exceeds the
// imported issue's content_revision (a corrupt or hand-edited dump) is dropped
// and left for the reconciler rather than inserted.
func TestEmbeddingImportDropsRowExceedingContentRevision(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	proj, err := src.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	iss, _, err := src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "t", Body: "b", Author: "a",
	})
	require.NoError(t, err)
	// content_revision is 0 (never edited); embed at 0 so the row is valid in
	// the source DB (the CHECK is import-only).
	fp := "a" + repeat63jsonl
	require.NoError(t, src.UpsertIssueEmbedding(ctx, db.IssueEmbedding{
		IssueID: iss.ID, EmbeddedContentRevision: 0, Fingerprint: fp, Dims: 2, Vector: []float32{1, 0},
	}))
	// Forge a future embedded_content_revision the imported issue can't match.
	_, err = src.ExecContext(ctx,
		`UPDATE issue_embeddings SET embedded_content_revision = 99 WHERE issue_id = ?`, iss.ID)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{}))

	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &buf, dst))

	var count int
	require.NoError(t, dst.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM issue_embeddings e
		  JOIN issues i ON i.id = e.issue_id
		 WHERE i.uid = ?`, iss.UID).Scan(&count))
	require.Equal(t, 0, count, "embedding exceeding content_revision must be dropped on import")
}
