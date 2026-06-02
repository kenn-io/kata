package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// CommentBodyByID returns the body of the comment with the given id.
// Used by the hooks dispatcher to resolve comment_body for
// issue.commented events at fire time.
func (d *Store) CommentBodyByID(ctx context.Context, id int64) (string, error) {
	var body string
	err := d.QueryRowContext(ctx, `SELECT body FROM comments WHERE id = ?`, id).Scan(&body)
	return body, err
}

// LatestAliasForProject returns the most-recently-seen alias for the
// project, if any. ok=false means the project has no aliases (the hook
// payload omits the alias block in that case).
func (d *Store) LatestAliasForProject(ctx context.Context, projectID int64) (db.AliasRow, bool, error) {
	var a db.AliasRow
	err := d.QueryRowContext(ctx,
		`SELECT alias_identity, alias_kind, root_path
		 FROM project_aliases WHERE project_id = ?
		 ORDER BY last_seen_at DESC LIMIT 1`, projectID).
		Scan(&a.Identity, &a.Kind, &a.RootPath)
	if errors.Is(err, sql.ErrNoRows) {
		return db.AliasRow{}, false, nil
	}
	if err != nil {
		return db.AliasRow{}, false, err
	}
	return a, true, nil
}

// LabelsForIssue returns sorted label values for the issue (alphabetical).
// Sorting is done in SQL so the result matches what the issue.created
// payload normalizes at insert time.
func (d *Store) LabelsForIssue(ctx context.Context, issueID int64) ([]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT label FROM issue_labels WHERE issue_id = ? ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
