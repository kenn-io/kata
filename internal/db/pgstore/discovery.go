package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// ReadyIssues returns actionable open issues that have no active blocker.
// Issues marked someday=true or scheduled after their effective local date or
// instant are parked and excluded. Optional owner and label filters are
// composed here so the same query remains useful to the CLI's different ready
// views.
func (s *Store) ReadyIssues(
	ctx context.Context,
	projectID int64,
	limit int,
	filter db.ReadyIssuesFilter,
) ([]db.Issue, error) {
	query := scheduledIssueSelect + `
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
	query += ` AND COALESCE((i.metadata::jsonb ->> 'someday')::boolean, false) = false`
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
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ready issues: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	var issues []db.Issue
	at := filter.At
	if at.IsZero() {
		at = time.Now()
	}
	for rows.Next() {
		issue, recurrenceTimezone, err := scanScheduledIssue(rows)
		if err != nil {
			return nil, err
		}
		due, err := metadata.ScheduledOnDue(
			string(issue.Metadata), at, scheduleDefaultTimezone(recurrenceTimezone, filter.DefaultTimezone),
		)
		if err != nil {
			return nil, fmt.Errorf("ready issue %s schedule: %w", issue.UID, err)
		}
		if !due {
			continue
		}
		issues = append(issues, issue)
		if limit > 0 && len(issues) == limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ready issues: %w", mapSQLError(err, nil))
	}
	return issues, nil
}

// ReadyIssuesGlobal returns actionable ready issues from all active projects
// along with the project name needed to render a qualified reference. Filter
// and parked-item semantics match ReadyIssues.
func (s *Store) ReadyIssuesGlobal(ctx context.Context, limit int, filter db.ReadyIssuesFilter) ([]db.ReadyGlobalIssue, error) {
	query := `SELECT ` + issueColumns + `, p.name, schedule_recurrence.timezone
  FROM issues i
  JOIN projects p ON p.id = i.project_id
	LEFT JOIN recurrences schedule_recurrence ON schedule_recurrence.id = i.recurrence_id
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
   )`
	args := []any{}
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	query += ` AND COALESCE((i.metadata::jsonb ->> 'someday')::boolean, false) = false`
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
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ready issues global: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()

	var issues []db.ReadyGlobalIssue
	at := filter.At
	if at.IsZero() {
		at = time.Now()
	}
	for rows.Next() {
		var buffer issueScanBuffer
		var projectName string
		var recurrenceTimezone sql.NullString
		destinations := append(buffer.destinations(), &projectName, &recurrenceTimezone)
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("scan ready global issue: %w", mapSQLError(err, nil))
		}
		issue, err := buffer.value()
		if err != nil {
			return nil, err
		}
		due, err := metadata.ScheduledOnDue(
			string(issue.Metadata), at,
			scheduleDefaultTimezone(recurrenceTimezone.String, filter.DefaultTimezone),
		)
		if err != nil {
			return nil, fmt.Errorf("ready global issue %s schedule: %w", issue.UID, err)
		}
		if !due {
			continue
		}
		issues = append(issues, db.ReadyGlobalIssue{Issue: issue, ProjectName: projectName})
		if limit > 0 && len(issues) == limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ready global issues: %w", mapSQLError(err, nil))
	}
	return issues, nil
}

func scheduleDefaultTimezone(recurrenceTimezone, daemonTimezone string) string {
	if recurrenceTimezone != "" {
		return recurrenceTimezone
	}
	return daemonTimezone
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
