package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"iter"

	"go.kenn.io/kata/internal/db"
)

// ExportIssueEmbeddings streams issue_embeddings rows joined to their issue
// UID, ordered by issue id. The join reuses the issue export's WHERE clause so
// a live-only (non-include-deleted) export omits any embedding whose parent
// issue is excluded, and a project-scoped export omits embeddings of issues in
// other projects. The vector is base64-encoded at the boundary.
func (d *Store) ExportIssueEmbeddings(ctx context.Context, f db.ExportFilter) iter.Seq2[db.IssueEmbeddingExport, error] {
	query := `SELECT i.uid, e.embedded_content_revision, e.embed_fingerprint, e.dims, e.vector_bytes
	          FROM issue_embeddings e
	          JOIN issues i ON i.id = e.issue_id` +
		exportWhere("i", f) + ` ORDER BY i.id ASC`
	return streamRows(ctx, d.readQ, "issue_embeddings", query, exportArgs(f),
		func(rows *sql.Rows) (db.IssueEmbeddingExport, error) {
			var rec db.IssueEmbeddingExport
			var raw []byte
			if err := rows.Scan(&rec.IssueUID, &rec.EmbeddedContentRevision, &rec.Fingerprint, &rec.Dims, &raw); err != nil {
				return db.IssueEmbeddingExport{}, scanError("issue_embedding", err)
			}
			rec.VectorB64 = base64.StdEncoding.EncodeToString(raw)
			return rec, nil
		})
}
