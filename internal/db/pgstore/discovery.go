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

const readyPriorityOrderBy = ` ORDER BY COALESCE(i.priority, 5), i.updated_at DESC, i.id DESC`

func buildReadyIssuesQuery(ctx context.Context, projectID int64, filter db.ReadyIssuesFilter, at time.Time) (string, []any) {
	args := []any{projectID}
	var query strings.Builder
	query.WriteString(scheduledIssueSelect + `
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
        AND ` + authorizedLinkPredicate(ctx, "l.", &args) + `
        AND blocker.status = 'open'
        AND blocker.deleted_at IS NULL
        AND blocker_project.deleted_at IS NULL
   )`)
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	appendAllowedIssueIDsPostgresBuilder(&query, &args, filter.AllowedIssueIDs)
	query.WriteString(" AND " + authorizedIssuePredicate(ctx, "i.project_id", &args))
	appendIssueScopePostgres(&query, &args, filter.IssueScope)
	query.WriteString(` AND COALESCE((i.metadata::jsonb ->> 'someday')::boolean, false) = false`)
	if filter.Unowned {
		query.WriteString(` AND (i.owner IS NULL OR i.assignment_expires_on <= ` + addArg(formatStoredTime(at)) + `)`)
	} else if filter.Owner != "" {
		query.WriteString(` AND i.owner = ` + addArg(filter.Owner) + ` AND (i.assignment_expires_on IS NULL OR i.assignment_expires_on > ` + addArg(formatStoredTime(at)) + `)`)
	}
	for _, label := range filter.Labels {
		query.WriteString(` AND EXISTS (
          SELECT 1 FROM issue_labels il
           WHERE il.issue_id = i.id AND il.label = ` + addArg(strings.ToLower(label)) + `)`)
	}
	for _, label := range filter.ExcludeLabels {
		query.WriteString(` AND NOT EXISTS (
          SELECT 1 FROM issue_labels il
           WHERE il.issue_id = i.id AND il.label = ` + addArg(strings.ToLower(label)) + `)`)
	}
	if filter.PriorityFirst {
		query.WriteString(readyPriorityOrderBy)
	} else {
		query.WriteString(` ORDER BY i.updated_at DESC, i.id DESC`)
	}
	return query.String(), args
}

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
	at := filter.At
	if at.IsZero() {
		at = time.Now()
	}
	query, args := buildReadyIssuesQuery(ctx, projectID, filter, at)
	return readReadyBatches(ctx, s, query, args, limit, func(rows *sql.Rows) (*db.Issue, error) {
		issue, recurrenceTimezone, err := scanScheduledIssue(ctx, rows)
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
			return nil, nil
		}
		if issue.AssignmentExpiresOn != nil && !issue.AssignmentExpiresOn.After(at) {
			issue.Owner = nil
			issue.AssignmentExpiresOn = nil
		}
		return &issue, nil
	})
}

func buildReadyIssuesGlobalQuery(ctx context.Context, filter db.ReadyIssuesFilter, at time.Time) (string, []any) {
	args := []any{}
	var query strings.Builder
	query.WriteString(`SELECT ` + issueColumns + `, p.name, schedule_recurrence.timezone
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
        AND ` + authorizedLinkPredicate(ctx, "l.", &args) + `
        AND blocker.status = 'open'
        AND blocker.deleted_at IS NULL
        AND blocker_project.deleted_at IS NULL
   )`)
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	appendAllowedIssueIDsPostgresBuilder(&query, &args, filter.AllowedIssueIDs)
	query.WriteString(" AND " + authorizedIssuePredicate(ctx, "i.project_id", &args))
	appendIssueScopePostgres(&query, &args, filter.IssueScope)
	query.WriteString(` AND COALESCE((i.metadata::jsonb ->> 'someday')::boolean, false) = false`)
	if filter.Unowned {
		query.WriteString(` AND (i.owner IS NULL OR i.assignment_expires_on <= ` + addArg(formatStoredTime(at)) + `)`)
	} else if filter.Owner != "" {
		query.WriteString(` AND i.owner = ` + addArg(filter.Owner) + ` AND (i.assignment_expires_on IS NULL OR i.assignment_expires_on > ` + addArg(formatStoredTime(at)) + `)`)
	}
	for _, label := range filter.Labels {
		query.WriteString(` AND EXISTS (
          SELECT 1 FROM issue_labels il
           WHERE il.issue_id = i.id AND il.label = ` + addArg(strings.ToLower(label)) + `)`)
	}
	for _, label := range filter.ExcludeLabels {
		query.WriteString(` AND NOT EXISTS (
          SELECT 1 FROM issue_labels il
           WHERE il.issue_id = i.id AND il.label = ` + addArg(strings.ToLower(label)) + `)`)
	}
	if filter.PriorityFirst {
		query.WriteString(readyPriorityOrderBy)
	} else {
		query.WriteString(` ORDER BY i.updated_at DESC, i.id DESC`)
	}
	return query.String(), args
}

// ReadyIssuesGlobal returns actionable ready issues from all active projects
// along with the project name needed to render a qualified reference. Filter
// and parked-item semantics match ReadyIssues.
func (s *Store) ReadyIssuesGlobal(ctx context.Context, limit int, filter db.ReadyIssuesFilter) ([]db.ReadyGlobalIssue, error) {
	at := filter.At
	if at.IsZero() {
		at = time.Now()
	}
	query, args := buildReadyIssuesGlobalQuery(ctx, filter, at)
	return readReadyBatches(ctx, s, query, args, limit, func(rows *sql.Rows) (*db.ReadyGlobalIssue, error) {
		var issue db.Issue
		var assignmentExpiresOn, closedAt, deletedAt storedNullTime
		var projectName string
		var recurrenceTimezone sql.NullString
		destinations := append(issueDestinations(&issue, &assignmentExpiresOn, &closedAt, &deletedAt), &projectName, &recurrenceTimezone)
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("scan ready global issue: %w", mapSQLError(err, nil))
		}
		issue.AssignmentExpiresOn = assignmentExpiresOn.Time
		issue.ClosedAt = closedAt.Time
		issue.DeletedAt = deletedAt.Time
		handle, _ := db.IssueTeammate(issue.Metadata)
		issue.SourceFallback(issue.Author, handle)
		db.RecordExposedIssue(ctx, issue.ID)
		due, err := metadata.ScheduledOnDue(
			string(issue.Metadata), at,
			scheduleDefaultTimezone(recurrenceTimezone.String, filter.DefaultTimezone),
		)
		if err != nil {
			return nil, fmt.Errorf("ready global issue %s schedule: %w", issue.UID, err)
		}
		if !due {
			return nil, nil
		}
		if issue.AssignmentExpiresOn != nil && !issue.AssignmentExpiresOn.After(at) {
			issue.Owner = nil
			issue.AssignmentExpiresOn = nil
		}
		return &db.ReadyGlobalIssue{Issue: issue, ProjectName: projectName}, nil
	})
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

// Schedules are evaluated in Go. Grow the SQL prefix when parked rows fill
// a batch, while keeping ordinary limited reads bounded on the wire.
func readReadyBatches[T any](ctx context.Context, s *Store, query string, args []any, limit int, scan func(*sql.Rows) (*T, error)) ([]T, error) {
	for batch := limit; ; batch *= 2 {
		bounded := query
		if batch > 0 {
			bounded += fmt.Sprintf(" LIMIT %d", batch)
		}
		rows, err := s.QueryContext(ctx, bounded, args...)
		if err != nil {
			return nil, fmt.Errorf("ready issues: %w", mapSQLError(err, nil))
		}
		var issues []T
		count := 0
		for rows.Next() {
			count++
			issue, err := scan(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			if issue != nil {
				issues = append(issues, *issue)
			}
			if limit > 0 && len(issues) == limit {
				break
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("iterate ready issues: %w", mapSQLError(err, nil))
		}
		if limit <= 0 || len(issues) == limit || count < batch {
			return issues, nil
		}
	}
}
