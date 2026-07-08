package sqlitestore

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// ListIssueContent returns live issues in live projects with id > afterID,
// ordered by id, limited. Soft-deleted issues are excluded: the feed's rows
// are sent to the configured embedding endpoint, and deleting an issue must
// stop that outbound flow — the mirror drops the row (and its vectors) at
// the next refresh, and a restore re-adds and re-embeds it. It is the vector
// mirror's feed: the caller pages with afterID until an empty page.
func (d *Store) ListIssueContent(ctx context.Context, afterID int64, limit int) ([]db.IssueContent, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := d.QueryContext(ctx, `
		SELECT i.id, i.uid, p.uid, i.title, i.body, i.content_revision
		FROM issues i
		JOIN projects p ON p.id = i.project_id
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND i.id > ?
		ORDER BY i.id ASC
		LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list issue content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.IssueContent
	for rows.Next() {
		var ic db.IssueContent
		if err := rows.Scan(&ic.ID, &ic.UID, &ic.ProjectUID, &ic.Title, &ic.Body, &ic.ContentRevision); err != nil {
			return nil, fmt.Errorf("scan issue content: %w", err)
		}
		out = append(out, ic)
	}
	return out, rows.Err()
}
