package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/db"
)

var _ db.UIStore = (*Store)(nil)

// UIEventCursor returns the durable high-water mark used by browser validators.
func (s *Store) UIEventCursor(ctx context.Context) (int64, error) {
	return maxUIEventID(ctx, s.DB)
}

// ReadUISnapshot captures all projection rows and the cursor in one read-only
// repeatable-read transaction.
func (s *Store) ReadUISnapshot(ctx context.Context, query db.UISnapshotQuery) (db.UISnapshotData, error) {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return db.UISnapshotData{}, fmt.Errorf("begin UI snapshot read: %w", mapSQLError(err, nil))
	}
	defer func() { _ = tx.Rollback() }()
	cursor, err := maxUIEventID(ctx, tx)
	if err != nil {
		return db.UISnapshotData{}, err
	}

	reuseAuthority := query.ReuseAuthorityCursor != nil && *query.ReuseAuthorityCursor == cursor
	projects := []db.UIProject{}
	var projectNames map[int64]string
	if reuseAuthority {
		projectNames, err = readUIProjectNames(ctx, tx)
	} else {
		projects, projectNames, err = readUIProjects(ctx, tx, s.uiProjectStatsRead)
	}
	if err != nil {
		return db.UISnapshotData{}, err
	}
	if s.uiReadStage != nil {
		if err := s.uiReadStage(ctx); err != nil {
			return db.UISnapshotData{}, fmt.Errorf("UI read stage: %w", err)
		}
	}
	data := db.UISnapshotData{
		Cursor: cursor, Projects: []db.UIProject{}, Issues: []db.UIIssue{},
		CollectionLinks: []db.UILink{},
		Comments:        []db.Comment{}, SelectedLabels: []db.IssueLabel{},
		SelectedLinks: []db.UILink{}, Recurrences: []db.Recurrence{},
		History: []db.Event{}, GraphIssues: []db.UIIssue{}, GraphLinks: []db.UILink{},
		GraphEdges: []db.UIGraphEdge{}, GraphUnresolvedRefs: []db.UIGraphUnresolvedRef{},
	}
	if reuseAuthority {
		data.AuthorityReused = true
	} else {
		data.Projects = projects
		data.Issues, err = readUIIssues(ctx, tx, query, projectNames)
		if err != nil {
			return db.UISnapshotData{}, err
		}
		data.CollectionLinks, err = readUICollectionLinks(ctx, tx, data.Issues, s.uiLinkDetailRead)
		if err != nil {
			return db.UISnapshotData{}, err
		}
	}
	if query.SelectedIssueUID != "" {
		selected, err := readUIIssueByUID(ctx, tx, query.SelectedIssueUID, projectNames)
		if err != nil {
			if !errors.Is(err, db.ErrNotFound) {
				return db.UISnapshotData{}, err
			}
			data.SelectedState, err = readUISelectedState(ctx, tx, query.SelectedIssueUID)
			if err != nil {
				return db.UISnapshotData{}, err
			}
		} else {
			data.SelectedState = "available"
			data.SelectedIssue = &selected
			data.Comments, err = readUIComments(ctx, tx, selected.ID)
			if err != nil {
				return db.UISnapshotData{}, err
			}
			data.SelectedLabels, err = readUIIssueLabels(ctx, tx, selected.ID)
			if err != nil {
				return db.UISnapshotData{}, err
			}
			data.SelectedLinks, err = readUILinksForIssue(ctx, tx, selected.ID)
			if err != nil {
				return db.UISnapshotData{}, err
			}
			if query.IncludeHistory {
				data.History, err = readUIHistory(ctx, tx, selected.UID)
				if err != nil {
					return db.UISnapshotData{}, err
				}
			}
		}
	}
	recurrenceProjectUID := query.ProjectUID
	if data.SelectedIssue != nil {
		recurrenceProjectUID = data.SelectedIssue.ProjectUID
	}
	if recurrenceProjectUID != "" {
		data.Recurrences, err = readUIRecurrences(ctx, tx, recurrenceProjectUID)
		if err != nil {
			return db.UISnapshotData{}, err
		}
	}
	if query.IncludeGraph {
		data.GraphIssues, err = readUIGraphIssues(ctx, tx, projectNames)
		if err != nil {
			return db.UISnapshotData{}, err
		}
		data.GraphLinks, err = readUIGraphLinks(ctx, tx, data.GraphIssues)
		if err != nil {
			return db.UISnapshotData{}, err
		}
		data.GraphEdges, data.GraphUnresolvedRefs, err = readUIGraphUnresolved(ctx, tx, data.GraphIssues)
		if err != nil {
			return db.UISnapshotData{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return db.UISnapshotData{}, fmt.Errorf("commit UI snapshot read: %w", mapSQLError(err, nil))
	}
	return data, nil
}

func readUIGraphIssues(
	ctx context.Context, tx *sql.Tx, projectNames map[int64]string,
) ([]db.UIIssue, error) {
	rows, err := tx.QueryContext(ctx, issueSelect+`
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> $1
		ORDER BY i.uid`, db.SystemProjectName)
	if err != nil {
		return nil, fmt.Errorf("read UI graph issues: %w", mapSQLError(err, nil))
	}
	issues := []db.UIIssue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		issues = append(issues, makeUIIssue(issue, projectNames[issue.ProjectID], nil))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate UI graph issues: %w", mapSQLError(err, nil))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close UI graph issues: %w", mapSQLError(err, nil))
	}
	if err := readUILabelsForIssues(ctx, tx, issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// ReadUIReferences captures bounded typeahead choices and their cursor in one
// read-only repeatable-read transaction.
func (s *Store) ReadUIReferences(ctx context.Context, query db.UIReferencesQuery) (db.UIReferencesData, error) {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return db.UIReferencesData{}, fmt.Errorf("begin UI references read: %w", mapSQLError(err, nil))
	}
	defer func() { _ = tx.Rollback() }()
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	projects, _, err := readUIProjects(ctx, tx, s.uiProjectStatsRead)
	if err != nil {
		return db.UIReferencesData{}, err
	}
	data := db.UIReferencesData{
		Projects: make([]db.Project, 0, min(limit, len(projects))),
		Issues:   []db.UIIssueReference{}, Owners: []string{}, Labels: []string{},
	}
	for _, project := range projects {
		if len(data.Projects) == limit {
			break
		}
		data.Projects = append(data.Projects, project.Project)
	}
	if s.uiReadStage != nil {
		if err := s.uiReadStage(ctx); err != nil {
			return db.UIReferencesData{}, fmt.Errorf("UI read stage: %w", err)
		}
	}
	data.Issues, err = readUIReferenceIssues(ctx, tx, query, limit)
	if err != nil {
		return db.UIReferencesData{}, err
	}
	data.Owners, err = readUIReferenceStrings(ctx, tx, `
		SELECT DISTINCT i.owner
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> $1
		  AND i.owner IS NOT NULL AND i.owner <> ''
		ORDER BY i.owner LIMIT $2`, db.SystemProjectName, limit)
	if err != nil {
		return db.UIReferencesData{}, err
	}
	data.Labels, err = readUIReferenceStrings(ctx, tx, `
		SELECT DISTINCT il.label
		FROM issue_labels il
		JOIN issues i ON i.id = il.issue_id
		JOIN projects p ON p.id = i.project_id
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> $1
		ORDER BY il.label LIMIT $2`, db.SystemProjectName, limit)
	if err != nil {
		return db.UIReferencesData{}, err
	}
	data.Cursor, err = maxUIEventID(ctx, tx)
	if err != nil {
		return db.UIReferencesData{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.UIReferencesData{}, fmt.Errorf("commit UI references read: %w", mapSQLError(err, nil))
	}
	return data, nil
}

type uiQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func maxUIEventID(ctx context.Context, queryer uiQueryer) (int64, error) {
	var cursor int64
	if err := queryer.QueryRowContext(ctx, `
		SELECT MAX(cursor) FROM (
			SELECT COALESCE(MAX(id), 0) AS cursor FROM events
			UNION ALL
			SELECT COALESCE(MAX(purge_reset_after_event_id), 0) FROM purge_log
			UNION ALL
			SELECT COALESCE(MAX(purge_reset_after_event_id), 0) FROM project_purge_log
		) AS cursors`).Scan(&cursor); err != nil {
		return 0, fmt.Errorf("read UI event cursor: %w", mapSQLError(err, nil))
	}
	return cursor, nil
}

func readUIProjects(
	ctx context.Context, tx *sql.Tx, onStatsRead func(),
) ([]db.UIProject, map[int64]string, error) {
	rows, err := tx.QueryContext(ctx, projectSelect+
		` WHERE deleted_at IS NULL AND name <> $1 ORDER BY name ASC`, db.SystemProjectName)
	if err != nil {
		return nil, nil, fmt.Errorf("read UI projects: %w", mapSQLError(err, nil))
	}
	projects := []db.UIProject{}
	projectNames := make(map[int64]string)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		projectNames[project.ID] = project.Name
		projects = append(projects, db.UIProject{Project: project})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("iterate UI projects: %w", mapSQLError(err, nil))
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close UI projects: %w", mapSQLError(err, nil))
	}
	if onStatsRead != nil {
		onStatsRead()
	}
	stats, err := readUIProjectStats(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	for idx := range projects {
		projects[idx].Stats = stats[projects[idx].Project.ID]
	}
	return projects, projectNames, nil
}

func readUIProjectNames(ctx context.Context, tx *sql.Tx) (map[int64]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name FROM projects WHERE deleted_at IS NULL AND name <> $1 ORDER BY name`,
		db.SystemProjectName)
	if err != nil {
		return nil, fmt.Errorf("read UI project names: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	projectNames := make(map[int64]string)
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan UI project name: %w", mapSQLError(err, nil))
		}
		projectNames[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate UI project names: %w", mapSQLError(err, nil))
	}
	return projectNames, nil
}

func readUIProjectStats(ctx context.Context, tx *sql.Tx) (map[int64]db.ProjectStats, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT p.id,
			COUNT(i.id) FILTER (WHERE i.status = 'open'),
			COUNT(i.id) FILTER (WHERE i.status = 'closed'),
			(SELECT e.created_at FROM events e WHERE e.project_id = p.id ORDER BY e.id DESC LIMIT 1)
		FROM projects p
		LEFT JOIN issues i ON i.project_id = p.id AND i.deleted_at IS NULL
		WHERE p.deleted_at IS NULL AND p.name <> $1
		GROUP BY p.id`, db.SystemProjectName)
	if err != nil {
		return nil, fmt.Errorf("read UI project stats: %w", mapSQLError(err, nil))
	}
	statsByProject := make(map[int64]db.ProjectStats)
	for rows.Next() {
		var projectID int64
		var stats db.ProjectStats
		var lastEventAt sql.NullString
		if err := rows.Scan(&projectID, &stats.Open, &stats.Closed, &lastEventAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan UI project stats: %w", mapSQLError(err, nil))
		}
		if lastEventAt.Valid {
			parsed, err := parseStoredTime(lastEventAt.String)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("parse UI project last event: %w", err)
			}
			stats.LastEventAt = &parsed
		}
		statsByProject[projectID] = stats
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate UI project stats: %w", mapSQLError(err, nil))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close UI project stats: %w", mapSQLError(err, nil))
	}
	return statsByProject, nil
}

func readUIIssues(ctx context.Context, tx *sql.Tx, query db.UISnapshotQuery,
	projectNames map[int64]string,
) ([]db.UIIssue, error) {
	statement := issueSelect + ` WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> $1`
	args := []any{db.SystemProjectName}
	if query.ProjectUID != "" {
		args = append(args, query.ProjectUID)
		statement += fmt.Sprintf(` AND p.uid = $%d`, len(args))
	}
	statuses := uiFilterValues(query.Statuses, query.Status)
	if len(statuses) == 0 {
		switch query.View {
		case "all-open", "inbox", "today", "upcoming", "deadlines":
			statuses = []string{"open"}
		case "logbook":
			statuses = []string{"closed"}
		}
	}
	if len(statuses) > 0 && !slices.Contains(statuses, "all") {
		statusPredicates := []string{}
		persistedStatuses := []string{}
		for _, status := range statuses {
			if status == "ready" {
				statusPredicates = append(statusPredicates, `(
					i.status = 'open' AND NOT EXISTS (
						SELECT 1 FROM links ready_link
						JOIN issues blocker ON blocker.id = ready_link.from_issue_id
						JOIN projects blocker_project ON blocker_project.id = blocker.project_id
						WHERE ready_link.type = 'blocks' AND ready_link.to_issue_id = i.id
						AND blocker.status = 'open' AND blocker.deleted_at IS NULL
						AND blocker_project.deleted_at IS NULL
					)
				)`)
			} else {
				persistedStatuses = append(persistedStatuses, status)
			}
		}
		if len(persistedStatuses) > 0 {
			statusPredicates = append(statusPredicates, `i.status IN (`+uiPostgresArgs(&args, persistedStatuses)+`)`)
		}
		statement += ` AND (` + strings.Join(statusPredicates, " OR ") + `)`
	}
	switch query.View {
	case "inbox":
		statement += ` AND p.metadata::jsonb ->> 'role' = 'inbox'`
	case "today":
		args = append(args, query.LocalDate, query.LocalDate)
		statement += fmt.Sprintf(` AND (LEFT(i.metadata::jsonb ->> 'scheduled_on', 10) <= $%d`+
			` OR LEFT(i.metadata::jsonb ->> 'deadline_on', 10) <= $%d)`, len(args)-1, len(args))
	case "upcoming":
		args = append(args, query.LocalDate)
		statement += fmt.Sprintf(` AND LEFT(i.metadata::jsonb ->> 'scheduled_on', 10) > $%d`, len(args))
	case "deadlines":
		statement += ` AND i.metadata::jsonb ->> 'deadline_on' IS NOT NULL`
	}
	owners := uiFilterValues(query.Owners, query.Owner)
	if len(owners) > 0 {
		statement += ` AND i.owner IN (` + uiPostgresArgs(&args, owners) + `)`
	}
	labels := uiFilterValues(query.Labels, query.Label)
	if len(labels) > 0 {
		statement += ` AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label IN (` +
			uiPostgresArgs(&args, labels) + `))`
	}
	if query.Text != "" {
		needle := "%" + strings.ToLower(query.Text) + "%"
		args = append(args, needle, needle, needle)
		statement += fmt.Sprintf(` AND (LOWER(i.title) LIKE $%d OR LOWER(i.body) LIKE $%d OR LOWER(i.short_id) LIKE $%d)`,
			len(args)-2, len(args)-1, len(args))
	}
	if len(query.Relationships) > 0 {
		predicates := uiPostgresRelationshipPredicates(query.Relationships)
		statement += ` AND (` + strings.Join(predicates, " OR ") + `)`
	}
	limit := query.Limit
	if limit > 1000 {
		limit = 1000
	}
	statement += ` ORDER BY i.updated_at DESC, i.id DESC`
	if limit > 0 {
		args = append(args, limit)
		statement += fmt.Sprintf(` LIMIT $%d`, len(args)) // #nosec G202 -- only a generated placeholder number is interpolated.
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI issues: %w", mapSQLError(err, nil))
	}
	issues := []db.UIIssue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		issues = append(issues, makeUIIssue(issue, projectNames[issue.ProjectID], nil))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate UI issues: %w", mapSQLError(err, nil))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close UI issues: %w", mapSQLError(err, nil))
	}
	if err := readUILabelsForIssues(ctx, tx, issues); err != nil {
		return nil, err
	}
	return issues, nil
}

func readUILabelsForIssues(ctx context.Context, tx *sql.Tx, issues []db.UIIssue) error {
	if len(issues) == 0 {
		return nil
	}
	indexByID := make(map[int64]int, len(issues))
	issueIDs := make([]int64, len(issues))
	for idx := range issues {
		issues[idx].Labels = []string{}
		issueIDs[idx] = issues[idx].ID
		indexByID[issues[idx].ID] = idx
	}
	rows, err := tx.QueryContext(ctx, `SELECT issue_id, label FROM issue_labels
		WHERE issue_id = ANY($1::bigint[]) ORDER BY issue_id, label`, issueIDs)
	if err != nil {
		return fmt.Errorf("read UI label batch: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var issueID int64
		var label string
		if err := rows.Scan(&issueID, &label); err != nil {
			return fmt.Errorf("scan UI label batch: %w", mapSQLError(err, nil))
		}
		idx, ok := indexByID[issueID]
		if ok {
			issues[idx].Labels = append(issues[idx].Labels, label)
		}
	}
	return mapSQLError(rows.Err(), nil)
}

func uiFilterValues(values []string, single string) []string {
	if len(values) > 0 {
		return values
	}
	if single != "" {
		return []string{single}
	}
	return nil
}

func uiPostgresRelationshipPredicates(relationships []string) []string {
	predicates := make([]string, 0, len(relationships))
	for _, relationship := range relationships {
		switch relationship {
		case "parent":
			predicates = append(predicates, `EXISTS (SELECT 1 FROM links relation JOIN issues peer ON peer.id = relation.to_issue_id JOIN projects peer_project ON peer_project.id = peer.project_id WHERE relation.type = 'parent' AND relation.from_issue_id = i.id AND peer.deleted_at IS NULL AND peer_project.deleted_at IS NULL)`)
		case "child":
			predicates = append(predicates, `EXISTS (SELECT 1 FROM links relation JOIN issues peer ON peer.id = relation.from_issue_id JOIN projects peer_project ON peer_project.id = peer.project_id WHERE relation.type = 'parent' AND relation.to_issue_id = i.id AND peer.deleted_at IS NULL AND peer_project.deleted_at IS NULL)`)
		case "blocks":
			predicates = append(predicates, `EXISTS (SELECT 1 FROM links relation JOIN issues peer ON peer.id = relation.to_issue_id JOIN projects peer_project ON peer_project.id = peer.project_id WHERE relation.type = 'blocks' AND relation.from_issue_id = i.id AND peer.deleted_at IS NULL AND peer_project.deleted_at IS NULL)`)
		case "blocked_by":
			predicates = append(predicates, `EXISTS (SELECT 1 FROM links relation JOIN issues peer ON peer.id = relation.from_issue_id JOIN projects peer_project ON peer_project.id = peer.project_id WHERE relation.type = 'blocks' AND relation.to_issue_id = i.id AND peer.deleted_at IS NULL AND peer_project.deleted_at IS NULL)`)
		case "related":
			predicates = append(predicates, `EXISTS (SELECT 1 FROM links relation JOIN issues source ON source.id = relation.from_issue_id JOIN projects source_project ON source_project.id = source.project_id JOIN issues target ON target.id = relation.to_issue_id JOIN projects target_project ON target_project.id = target.project_id WHERE relation.type = 'related' AND (relation.from_issue_id = i.id OR relation.to_issue_id = i.id) AND source.deleted_at IS NULL AND target.deleted_at IS NULL AND source_project.deleted_at IS NULL AND target_project.deleted_at IS NULL)`)
		}
	}
	return predicates
}

func uiPostgresArgs(args *[]any, values []string) string {
	placeholders := make([]string, 0, len(values))
	for _, value := range values {
		*args = append(*args, value)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(*args)))
	}
	return strings.Join(placeholders, ",")
}

func readUIIssueByUID(ctx context.Context, tx *sql.Tx, issueUID string,
	projectNames map[int64]string,
) (db.UIIssue, error) {
	issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+
		` WHERE i.uid = $1 AND i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> $2`,
		issueUID, db.SystemProjectName))
	if err != nil {
		return db.UIIssue{}, err
	}
	labels, err := readUILabelStrings(ctx, tx, issue.ID)
	if err != nil {
		return db.UIIssue{}, err
	}
	return makeUIIssue(issue, projectNames[issue.ProjectID], labels), nil
}

func readUISelectedState(ctx context.Context, tx *sql.Tx, issueUID string) (string, error) {
	var archived bool
	err := tx.QueryRowContext(ctx, `
		SELECT i.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.uid = $1`, issueUID).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return "missing", nil
	}
	if err != nil {
		return "", fmt.Errorf("read selected UI state: %w", mapSQLError(err, nil))
	}
	if archived {
		return "archived", nil
	}
	return "missing", nil
}

func makeUIIssue(issue db.Issue, projectName string, labels []string) db.UIIssue {
	if labels == nil {
		labels = []string{}
	}
	return db.UIIssue{Issue: issue, ProjectName: projectName,
		QualifiedID: projectName + "#" + issue.ShortID, Labels: labels}
}

func readUILabelStrings(ctx context.Context, tx *sql.Tx, issueID int64) ([]string, error) {
	return readUIReferenceStrings(ctx, tx,
		`SELECT label FROM issue_labels WHERE issue_id = $1 ORDER BY label`, issueID)
}

func readUIComments(ctx context.Context, tx *sql.Tx, issueID int64) ([]db.Comment, error) {
	rows, err := tx.QueryContext(ctx, commentSelect+` WHERE issue_id = $1 ORDER BY created_at ASC, id ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("read UI comments: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	comments := []db.Comment{}
	for rows.Next() {
		comment, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
	}
	return comments, mapSQLError(rows.Err(), nil)
}

func readUIIssueLabels(ctx context.Context, tx *sql.Tx, issueID int64) ([]db.IssueLabel, error) {
	rows, err := tx.QueryContext(ctx, labelSelect+` WHERE issue_id = $1 ORDER BY label`, issueID)
	if err != nil {
		return nil, fmt.Errorf("read selected UI labels: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	labels := []db.IssueLabel{}
	for rows.Next() {
		label, err := scanLabel(rows)
		if err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, mapSQLError(rows.Err(), nil)
}

func readUILinksForIssue(ctx context.Context, tx *sql.Tx, issueID int64) ([]db.UILink, error) {
	rows, err := tx.QueryContext(ctx, linkSelect+
		` WHERE (from_issue_id = $1 OR to_issue_id = $1)
		AND from_issue_id IN (SELECT endpoint.id FROM issues endpoint JOIN projects endpoint_project ON endpoint_project.id = endpoint.project_id WHERE endpoint.deleted_at IS NULL AND endpoint_project.deleted_at IS NULL)
		AND to_issue_id IN (SELECT endpoint.id FROM issues endpoint JOIN projects endpoint_project ON endpoint_project.id = endpoint.project_id WHERE endpoint.deleted_at IS NULL AND endpoint_project.deleted_at IS NULL)
		ORDER BY id`, issueID)
	if err != nil {
		return nil, fmt.Errorf("read selected UI links: %w", mapSQLError(err, nil))
	}
	links, err := collectUILinks(rows)
	if err != nil {
		return nil, err
	}
	return enrichUILinks(ctx, tx, links)
}

func collectUILinks(rows *sql.Rows) ([]db.Link, error) {
	defer func() { _ = rows.Close() }()
	links := []db.Link{}
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close UI links: %w", mapSQLError(err, nil))
	}
	return links, nil
}

func enrichUILinks(ctx context.Context, tx *sql.Tx, links []db.Link) ([]db.UILink, error) {
	enriched := make([]db.UILink, 0, len(links))
	for _, link := range links {
		var fromProject, fromShort, fromStatus, toProject, toShort, toStatus string
		if err := tx.QueryRowContext(ctx, `
			SELECT fp.name, fi.short_id, fi.status, tp.name, ti.short_id, ti.status
			FROM issues fi JOIN projects fp ON fp.id = fi.project_id
			JOIN issues ti ON ti.id = $1 JOIN projects tp ON tp.id = ti.project_id
			WHERE fi.id = $2`, link.ToIssueID, link.FromIssueID).Scan(
			&fromProject, &fromShort, &fromStatus, &toProject, &toShort, &toStatus); err != nil {
			return nil, fmt.Errorf("enrich UI link: %w", mapSQLError(err, nil))
		}
		enriched = append(enriched, db.UILink{
			Link: link, FromQualifiedID: fromProject + "#" + fromShort, FromStatus: fromStatus,
			ToQualifiedID: toProject + "#" + toShort, ToStatus: toStatus,
		})
	}
	return enriched, nil
}

func readUIRecurrences(ctx context.Context, tx *sql.Tx, projectUID string) ([]db.Recurrence, error) {
	statement := recurrenceSelect + ` JOIN projects p ON p.id = r.project_id
		WHERE r.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> $1`
	args := []any{db.SystemProjectName}
	if projectUID != "" {
		args = append(args, projectUID)
		statement += fmt.Sprintf(` AND p.uid = $%d`, len(args)) // #nosec G202 -- only a generated placeholder number is interpolated.
	}
	statement += ` ORDER BY r.created_at DESC, r.id DESC`
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI recurrences: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	recurrences := []db.Recurrence{}
	for rows.Next() {
		recurrence, err := scanRecurrence(rows)
		if err != nil {
			return nil, err
		}
		recurrences = append(recurrences, recurrence)
	}
	return recurrences, mapSQLError(rows.Err(), nil)
}

func readUIHistory(ctx context.Context, tx *sql.Tx, issueUID string) ([]db.Event, error) {
	rows, err := tx.QueryContext(ctx, eventSelect+
		` WHERE e.issue_uid = $1 OR e.related_issue_uid = $1 ORDER BY e.id DESC LIMIT 500`, issueUID)
	if err != nil {
		return nil, fmt.Errorf("read UI history: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	history := []db.Event{}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		history = append(history, event)
	}
	return history, mapSQLError(rows.Err(), nil)
}

func readUIGraphLinks(ctx context.Context, tx *sql.Tx, issues []db.UIIssue) ([]db.UILink, error) {
	if len(issues) == 0 {
		return []db.UILink{}, nil
	}
	args := make([]any, 0, len(issues)*2)
	fromPlaceholders := make([]string, len(issues))
	toPlaceholders := make([]string, len(issues))
	for idx, issue := range issues {
		args = append(args, issue.ID)
		fromPlaceholders[idx] = fmt.Sprintf("$%d", len(args))
	}
	for idx, issue := range issues {
		args = append(args, issue.ID)
		toPlaceholders[idx] = fmt.Sprintf("$%d", len(args))
	}
	rows, err := tx.QueryContext(ctx, linkSelect+` WHERE from_issue_id IN (`+
		strings.Join(fromPlaceholders, ",")+`) AND to_issue_id IN (`+
		strings.Join(toPlaceholders, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI graph links: %w", mapSQLError(err, nil))
	}
	links, err := collectUILinks(rows)
	if err != nil {
		return nil, err
	}
	return enrichUILinks(ctx, tx, links)
}

func readUIGraphUnresolved(
	ctx context.Context, tx *sql.Tx, issues []db.UIIssue,
) ([]db.UIGraphEdge, []db.UIGraphUnresolvedRef, error) {
	if len(issues) == 0 {
		return []db.UIGraphEdge{}, []db.UIGraphUnresolvedRef{}, nil
	}
	visible := make(map[int64]struct{}, len(issues))
	args := make([]any, 0, len(issues)*2)
	fromPlaceholders := make([]string, len(issues))
	toPlaceholders := make([]string, len(issues))
	for idx, issue := range issues {
		visible[issue.ID] = struct{}{}
		args = append(args, issue.ID)
		fromPlaceholders[idx] = fmt.Sprintf("$%d", len(args))
	}
	for idx, issue := range issues {
		args = append(args, issue.ID)
		toPlaceholders[idx] = fmt.Sprintf("$%d", len(args))
	}
	rows, err := tx.QueryContext(ctx, linkSelect+` WHERE from_issue_id IN (`+
		strings.Join(fromPlaceholders, ",")+`) OR to_issue_id IN (`+
		strings.Join(toPlaceholders, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("read unresolved UI graph links: %w", mapSQLError(err, nil))
	}
	links, err := collectUILinks(rows)
	if err != nil {
		return nil, nil, err
	}
	edges := []db.UIGraphEdge{}
	refs := []db.UIGraphUnresolvedRef{}
	for _, link := range links {
		fromID, toID := link.FromIssueID, link.ToIssueID
		fromUID, toUID := link.FromIssueUID, link.ToIssueUID
		if link.Type == "parent" {
			fromID, toID = toID, fromID
			fromUID, toUID = toUID, fromUID
		}
		_, fromVisible := visible[fromID]
		_, toVisible := visible[toID]
		if fromVisible == toVisible {
			continue
		}
		missingID, missingUID, side, otherUID := fromID, fromUID, "from", toUID
		if fromVisible {
			missingID, missingUID, side, otherUID = toID, toUID, "to", fromUID
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM issues WHERE id = $1`, missingID).Scan(&exists); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("check unresolved UI graph endpoint: %w", mapSQLError(err, nil))
		}
		edges = append(edges, db.UIGraphEdge{
			FromUID: fromUID, ToUID: toUID, Kind: link.Type, Layout: true,
		})
		refs = append(refs, db.UIGraphUnresolvedRef{
			UID: missingUID, Side: side, Kind: link.Type, OtherUID: otherUID,
		})
	}
	return edges, refs, nil
}

func readUICollectionLinks(
	ctx context.Context, tx *sql.Tx, issues []db.UIIssue, onDetailRead func(),
) ([]db.UILink, error) {
	if len(issues) == 0 {
		return []db.UILink{}, nil
	}
	args := make([]any, 0, len(issues)*2)
	fromPlaceholders := make([]string, len(issues))
	toPlaceholders := make([]string, len(issues))
	for idx, issue := range issues {
		args = append(args, issue.ID)
		fromPlaceholders[idx] = fmt.Sprintf("$%d", len(args))
	}
	for idx, issue := range issues {
		args = append(args, issue.ID)
		toPlaceholders[idx] = fmt.Sprintf("$%d", len(args))
	}
	if onDetailRead != nil {
		onDetailRead()
	}
	rows, err := tx.QueryContext(ctx, uiLinkSelect+` WHERE (l.from_issue_id IN (`+
		strings.Join(fromPlaceholders, ",")+`) OR l.to_issue_id IN (`+
		strings.Join(toPlaceholders, ",")+`))
		AND fi.deleted_at IS NULL AND fp.deleted_at IS NULL
		AND ti.deleted_at IS NULL AND tp.deleted_at IS NULL
		ORDER BY l.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI collection links: %w", mapSQLError(err, nil))
	}
	return collectUIDetailedLinks(rows)
}

const uiLinkSelect = `SELECT l.id, l.from_issue_id, l.from_issue_uid,
	l.to_issue_id, l.to_issue_uid, l.type, l.author, l.created_at,
	fp.name, fi.short_id, fi.status, tp.name, ti.short_id, ti.status
	FROM links l
	JOIN issues fi ON fi.id = l.from_issue_id
	JOIN projects fp ON fp.id = fi.project_id
	JOIN issues ti ON ti.id = l.to_issue_id
	JOIN projects tp ON tp.id = ti.project_id`

func collectUIDetailedLinks(rows *sql.Rows) ([]db.UILink, error) {
	defer func() { _ = rows.Close() }()
	links := []db.UILink{}
	for rows.Next() {
		var link db.UILink
		var fromProject, fromShort, toProject, toShort string
		var createdAt string
		if err := rows.Scan(
			&link.ID, &link.FromIssueID, &link.FromIssueUID,
			&link.ToIssueID, &link.ToIssueUID, &link.Type, &link.Author, &createdAt,
			&fromProject, &fromShort, &link.FromStatus, &toProject, &toShort, &link.ToStatus,
		); err != nil {
			return nil, fmt.Errorf("scan detailed UI link: %w", mapSQLError(err, nil))
		}
		var err error
		link.CreatedAt, err = parseStoredTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse detailed UI link created_at: %w", err)
		}
		link.FromQualifiedID = fromProject + "#" + fromShort
		link.ToQualifiedID = toProject + "#" + toShort
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	return links, mapSQLError(rows.Close(), nil)
}

func readUIReferenceIssues(ctx context.Context, tx *sql.Tx, query db.UIReferencesQuery,
	limit int,
) ([]db.UIIssueReference, error) {
	statement := `SELECT i.uid, p.uid, p.name, i.short_id, i.title, i.status
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> $1`
	args := []any{db.SystemProjectName}
	if query.ProjectUID != "" {
		args = append(args, query.ProjectUID)
		statement += fmt.Sprintf(` AND p.uid = $%d`, len(args))
	}
	if query.Query != "" {
		needle := "%" + strings.ToLower(query.Query) + "%"
		args = append(args, needle, needle, needle, needle)
		statement += fmt.Sprintf(` AND (LOWER(i.title) LIKE $%d OR LOWER(i.short_id) LIKE $%d OR LOWER(p.name) LIKE $%d OR LOWER(p.name || '#' || i.short_id) LIKE $%d)`,
			len(args)-3, len(args)-2, len(args)-1, len(args))
	}
	args = append(args, limit)
	statement += fmt.Sprintf(` ORDER BY p.name, i.short_id LIMIT $%d`, len(args)) // #nosec G202 -- only a generated placeholder number is interpolated.
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI issue references: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	issues := []db.UIIssueReference{}
	for rows.Next() {
		var issue db.UIIssueReference
		if err := rows.Scan(&issue.UID, &issue.ProjectUID, &issue.ProjectName,
			&issue.ShortID, &issue.Title, &issue.Status); err != nil {
			return nil, fmt.Errorf("scan UI issue reference: %w", mapSQLError(err, nil))
		}
		issue.QualifiedID = issue.ProjectName + "#" + issue.ShortID
		issues = append(issues, issue)
	}
	return issues, mapSQLError(rows.Err(), nil)
}

func readUIReferenceStrings(ctx context.Context, tx *sql.Tx, statement string,
	args ...any,
) ([]string, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI reference choices: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scan UI reference choice: %w", mapSQLError(err, nil))
		}
		values = append(values, value)
	}
	return values, mapSQLError(rows.Err(), nil)
}
