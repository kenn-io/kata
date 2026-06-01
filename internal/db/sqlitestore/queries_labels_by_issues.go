package sqlitestore

import (
	"context"
	"fmt"
	"strings"
)

// labelsByIssuesChunkSize bounds the IN-clause width per query. SQLite's
// SQLITE_LIMIT_VARIABLE_NUMBER defaults to 32766 in modern builds and as
// low as 999 in older ones; 500 stays comfortably under both with one
// extra parameter slot for project_id. Chunking trades one query for
// ceil(N/500) queries on large projects in exchange for safety against
// 500-class errors on list pages with limit=0 / unbounded results.
const labelsByIssuesChunkSize = 500

// LabelsByIssues returns a map of issueID → []label for every issue in
// issueIDs that belongs to projectID. Labels per issue are sorted
// alphabetically; issues with no labels are absent from the map (callers
// should treat a missing key as "no labels"). Empty input short-circuits
// without a SQL roundtrip.
//
// Constrained by both project_id (via JOIN through issues) and id IN (...)
// for cross-project safety: a caller passing an issueID that belongs to a
// different project gets no rows for that ID rather than leaking labels
// across projects. The issue_labels table itself has no project_id
// column (see schema.sql) — projection has to go through
// issues.project_id.
//
// Chunked into groups of labelsByIssuesChunkSize to stay under SQLite's
// bound-parameter limit (roborev job 246). The list endpoint allows
// limit=0 / unbounded results, so callers can pass arbitrarily many IDs;
// the previous single-shot IN clause turned a >999-issue list page into
// a 500 on builds with the older SQLITE_LIMIT_VARIABLE_NUMBER default.
// Per-issue ordering is preserved by sorting on (issue_id, label) within
// each chunk and by appending into the same map across chunks.
func (d *Store) LabelsByIssues(
	ctx context.Context, projectID int64, issueIDs []int64,
) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	for i := 0; i < len(issueIDs); i += labelsByIssuesChunkSize {
		end := i + labelsByIssuesChunkSize
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		if err := d.appendLabelsForChunk(ctx, projectID, issueIDs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// appendLabelsForChunk runs the LabelsByIssues query for one chunk of
// issue IDs and merges results into out. Extracted to keep the chunking
// loop readable and to bound function complexity per the project's
// 8-cyclomatic / 100-line limits.
func (d *Store) appendLabelsForChunk(
	ctx context.Context, projectID int64, chunk []int64, out map[int64][]string,
) error {
	placeholders := make([]string, len(chunk))
	args := make([]interface{}, 0, len(chunk)+1)
	args = append(args, projectID)
	for i, id := range chunk {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `SELECT il.issue_id, il.label
	          FROM issue_labels il
	          JOIN issues i ON i.id = il.issue_id
	          WHERE i.project_id = ?
	            AND il.issue_id IN (` + strings.Join(placeholders, ",") + `)
	          ORDER BY il.issue_id ASC, il.label ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("labels by issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			issueID int64
			label   string
		)
		if err := rows.Scan(&issueID, &label); err != nil {
			return fmt.Errorf("scan labels by issues: %w", err)
		}
		out[issueID] = append(out[issueID], label)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate labels by issues: %w", err)
	}
	return nil
}
