package pgstore

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// ReadyIssues returns open issues that have no active blocker. Optional owner
// and label filters are composed here so the same query remains useful to the
// CLI's different ready views.
func (s *Store) ReadyIssues(
	ctx context.Context,
	projectID int64,
	limit int,
	filter db.ReadyIssuesFilter,
) ([]db.Issue, error) {
	query := issueSelect + `
 WHERE i.project_id = $1
   AND i.status = 'open'
   AND i.deleted_at IS NULL
   AND NOT EXISTS (
     SELECT 1
       FROM links l
       JOIN issues blocker ON blocker.id = l.from_issue_id
       JOIN projects blocker_project ON blocker_project.id = blocker.project_id
      WHERE l.type = 'blocks'
        AND l.to_issue_id = i.id
        AND blocker.status = 'open'
        AND blocker.deleted_at IS NULL
        AND blocker_project.deleted_at IS NULL
   )`
	args := []any{projectID}
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if filter.Unowned {
		query += ` AND i.owner IS NULL`
	} else if filter.Owner != "" {
		query += ` AND i.owner = ` + addArg(filter.Owner)
	}
	for _, label := range filter.Labels {
		query += ` AND EXISTS (
          SELECT 1 FROM issue_labels il
           WHERE il.issue_id = i.id AND il.label = ` + addArg(strings.ToLower(label)) + `)`
	}
	for _, label := range filter.ExcludeLabels {
		query += ` AND NOT EXISTS (
          SELECT 1 FROM issue_labels il
           WHERE il.issue_id = i.id AND il.label = ` + addArg(strings.ToLower(label)) + `)`
	}
	query += ` ORDER BY i.updated_at DESC, i.id DESC`
	if limit > 0 {
		query += ` LIMIT ` + addArg(limit)
	}

	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ready issues: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	var issues []db.Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ready issues: %w", mapSQLError(err, nil))
	}
	return issues, nil
}

// ReadyIssuesGlobal returns ready issues from all active projects along with
// the project name needed to render a qualified reference.
func (s *Store) ReadyIssuesGlobal(ctx context.Context, limit int) ([]db.ReadyGlobalIssue, error) {
	query := `SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision, i.recurrence_id,
       i.occurrence_key, i.created_at, i.updated_at, i.closed_at, i.deleted_at, p.name
  FROM issues i
  JOIN projects p ON p.id = i.project_id
 WHERE i.status = 'open'
   AND i.deleted_at IS NULL
   AND p.deleted_at IS NULL
   AND NOT EXISTS (
     SELECT 1
       FROM links l
       JOIN issues blocker ON blocker.id = l.from_issue_id
       JOIN projects blocker_project ON blocker_project.id = blocker.project_id
      WHERE l.type = 'blocks'
        AND l.to_issue_id = i.id
        AND blocker.status = 'open'
        AND blocker.deleted_at IS NULL
        AND blocker_project.deleted_at IS NULL
   )
 ORDER BY i.updated_at DESC, i.id DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT $1`
		args = append(args, limit)
	}
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ready issues global: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()

	var issues []db.ReadyGlobalIssue
	for rows.Next() {
		var buffer issueScanBuffer
		var projectName string
		destinations := append(buffer.destinations(), &projectName)
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("scan ready global issue: %w", mapSQLError(err, nil))
		}
		issue, err := buffer.value()
		if err != nil {
			return nil, err
		}
		issues = append(issues, db.ReadyGlobalIssue{Issue: issue, ProjectName: projectName})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ready global issues: %w", mapSQLError(err, nil))
	}
	return issues, nil
}

// IssueQualifiersByUIDs resolves stable issue UIDs to their current project
// and short ID. Unknown UIDs are intentionally omitted.
func (s *Store) IssueQualifiersByUIDs(
	ctx context.Context,
	uids []string,
) (map[string]db.IssueQualifier, error) {
	qualifiers := make(map[string]db.IssueQualifier)
	if len(uids) == 0 {
		return qualifiers, nil
	}
	rows, err := s.QueryContext(ctx, `SELECT i.uid, i.project_id, p.name, i.short_id
  FROM issues i
  JOIN projects p ON p.id = i.project_id
 WHERE i.uid = ANY($1::text[])`, uids)
	if err != nil {
		return nil, fmt.Errorf("issue qualifiers by uids: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid string
		var qualifier db.IssueQualifier
		if err := rows.Scan(&uid, &qualifier.ProjectID, &qualifier.ProjectName, &qualifier.ShortID); err != nil {
			return nil, fmt.Errorf("scan issue qualifier: %w", mapSQLError(err, nil))
		}
		qualifiers[uid] = qualifier
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue qualifiers: %w", mapSQLError(err, nil))
	}
	return qualifiers, nil
}

// ListIssueContent pages live issue text for the vector mirror.
func (s *Store) ListIssueContent(ctx context.Context, afterID int64, limit int) ([]db.IssueContent, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.QueryContext(ctx, `SELECT i.id, i.uid, p.uid, i.title, i.body, i.content_revision
  FROM issues i
  JOIN projects p ON p.id = i.project_id
 WHERE i.deleted_at IS NULL
   AND p.deleted_at IS NULL
   AND i.id > $1
 ORDER BY i.id ASC
 LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list issue content: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	var content []db.IssueContent
	for rows.Next() {
		var issue db.IssueContent
		if err := rows.Scan(
			&issue.ID, &issue.UID, &issue.ProjectUID, &issue.Title, &issue.Body, &issue.ContentRevision,
		); err != nil {
			return nil, fmt.Errorf("scan issue content: %w", mapSQLError(err, nil))
		}
		content = append(content, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue content: %w", mapSQLError(err, nil))
	}
	return content, nil
}
