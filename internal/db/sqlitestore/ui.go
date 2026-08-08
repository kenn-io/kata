package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

var _ db.UIStore = (*Store)(nil)

// UIEventCursor returns the cheap durable high-water mark used for validators.
func (d *Store) UIEventCursor(ctx context.Context) (int64, error) {
	return maxUIEventID(ctx, d.DB)
}

// ReadUISnapshot captures browser projection rows and their cursor in one
// read-only transaction. Projection shaping starts only after every row has
// been collected from that transaction.
func (d *Store) ReadUISnapshot(ctx context.Context, query db.UISnapshotQuery) (db.UISnapshotData, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return db.UISnapshotData{}, fmt.Errorf("begin UI snapshot read: %w", err)
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
		projects, projectNames, err = readUIProjects(ctx, tx, d.uiProjectStatsRead)
	}
	if err != nil {
		return db.UISnapshotData{}, err
	}
	if d.uiReadStage != nil {
		if err := d.uiReadStage(ctx); err != nil {
			return db.UISnapshotData{}, fmt.Errorf("UI read stage: %w", err)
		}
	}
	data := db.UISnapshotData{
		Cursor:          cursor,
		Projects:        []db.UIProject{},
		Issues:          []db.UIIssue{},
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
		data.CollectionLinks, err = readUICollectionLinks(ctx, tx, data.Issues, d.uiLinkDetailRead)
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
		return db.UISnapshotData{}, fmt.Errorf("commit UI snapshot read: %w", err)
	}
	return data, nil
}

func readUIGraphIssues(
	ctx context.Context, tx *sql.Tx, projectNames map[int64]string,
) ([]db.UIIssue, error) {
	rows, err := tx.QueryContext(ctx, issueSelect+`
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> ?
		ORDER BY i.uid`, db.SystemProjectName)
	if err != nil {
		return nil, fmt.Errorf("read UI graph issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	issues := []db.UIIssue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, makeUIIssue(issue, projectNames[issue.ProjectID], nil))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate UI graph issues: %w", err)
	}
	if err := readUILabelsForIssues(ctx, tx, issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// ReadUIReferences captures bounded typeahead choices and their cursor in one
// read-only transaction.
func (d *Store) ReadUIReferences(ctx context.Context, query db.UIReferencesQuery) (db.UIReferencesData, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return db.UIReferencesData{}, fmt.Errorf("begin UI references read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	projects, _, err := readUIProjects(ctx, tx, d.uiProjectStatsRead)
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
	if d.uiReadStage != nil {
		if err := d.uiReadStage(ctx); err != nil {
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
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> ? AND i.owner IS NOT NULL AND i.owner <> ''
		ORDER BY i.owner LIMIT ?`, db.SystemProjectName, limit)
	if err != nil {
		return db.UIReferencesData{}, err
	}
	data.Labels, err = readUIReferenceStrings(ctx, tx, `
		SELECT DISTINCT il.label
		FROM issue_labels il
		JOIN issues i ON i.id = il.issue_id
		JOIN projects p ON p.id = i.project_id
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> ?
		ORDER BY il.label LIMIT ?`, db.SystemProjectName, limit)
	if err != nil {
		return db.UIReferencesData{}, err
	}
	data.Cursor, err = maxUIEventID(ctx, tx)
	if err != nil {
		return db.UIReferencesData{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.UIReferencesData{}, fmt.Errorf("commit UI references read: %w", err)
	}
	return data, nil
}

type uiQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
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
		)`).Scan(&cursor); err != nil {
		return 0, fmt.Errorf("read UI event cursor: %w", err)
	}
	return cursor, nil
}

func readUIProjects(
	ctx context.Context, tx *sql.Tx, onStatsRead func(),
) ([]db.UIProject, map[int64]string, error) {
	rows, err := tx.QueryContext(ctx, projectSelect+
		` WHERE deleted_at IS NULL AND name <> ? ORDER BY name ASC`, db.SystemProjectName)
	if err != nil {
		return nil, nil, fmt.Errorf("read UI projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	projects := []db.UIProject{}
	projectNames := make(map[int64]string)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, nil, err
		}
		projectNames[project.ID] = project.Name
		projects = append(projects, db.UIProject{Project: project})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate UI projects: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close UI projects: %w", err)
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
		`SELECT id, name FROM projects WHERE deleted_at IS NULL AND name <> ? ORDER BY name`,
		db.SystemProjectName)
	if err != nil {
		return nil, fmt.Errorf("read UI project names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	projectNames := make(map[int64]string)
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan UI project name: %w", err)
		}
		projectNames[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate UI project names: %w", err)
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
		WHERE p.deleted_at IS NULL AND p.name <> ?
		GROUP BY p.id`, db.SystemProjectName)
	if err != nil {
		return nil, fmt.Errorf("read UI project stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	statsByProject := make(map[int64]db.ProjectStats)
	for rows.Next() {
		var projectID int64
		var stats db.ProjectStats
		var lastEventAt sql.NullTime
		if err := rows.Scan(&projectID, &stats.Open, &stats.Closed, &lastEventAt); err != nil {
			return nil, fmt.Errorf("scan UI project stats: %w", err)
		}
		if lastEventAt.Valid {
			stats.LastEventAt = &lastEventAt.Time
		}
		statsByProject[projectID] = stats
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate UI project stats: %w", err)
	}
	return statsByProject, nil
}

func readUIIssues(
	ctx context.Context,
	tx *sql.Tx,
	query db.UISnapshotQuery,
	projectNames map[int64]string,
) ([]db.UIIssue, error) {
	statement := issueSelect + ` WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> ?`
	args := []any{db.SystemProjectName}
	if query.ProjectUID != "" {
		statement += ` AND p.uid = ?`
		args = append(args, query.ProjectUID)
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
				readyDate := query.ReadyDate
				if readyDate == "" {
					readyDate = time.Now().UTC().Format(time.DateOnly)
				}
				args = append(args, readyDate)
				statusPredicates = append(statusPredicates, `(
					i.status = 'open'
					AND COALESCE(json_extract(i.metadata, '$.someday'), 0) != 1
					AND (json_extract(i.metadata, '$.scheduled_on') IS NULL
						OR json_extract(i.metadata, '$.scheduled_on') <= ?)
					AND NOT EXISTS (
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
			statusPredicates = append(statusPredicates, `i.status IN (`+uiSQLitePlaceholders(len(persistedStatuses))+`)`)
			for _, status := range persistedStatuses {
				args = append(args, status)
			}
		}
		statement += ` AND (` + strings.Join(statusPredicates, " OR ") + `)`
	}
	switch query.View {
	case "inbox":
		statement += ` AND json_extract(p.metadata, '$.role') = 'inbox'`
	case "today":
		statement += ` AND (substr(json_extract(i.metadata, '$.scheduled_on'), 1, 10) <= ?` +
			` OR substr(json_extract(i.metadata, '$.deadline_on'), 1, 10) <= ?)`
		args = append(args, query.LocalDate, query.LocalDate)
	case "upcoming":
		statement += ` AND substr(json_extract(i.metadata, '$.scheduled_on'), 1, 10) > ?`
		args = append(args, query.LocalDate)
	case "deadlines":
		statement += ` AND json_extract(i.metadata, '$.deadline_on') IS NOT NULL`
	}
	owners := uiFilterValues(query.Owners, query.Owner)
	if len(owners) > 0 {
		statement += ` AND i.owner IN (` + uiSQLitePlaceholders(len(owners)) + `)`
		for _, owner := range owners {
			args = append(args, owner)
		}
	}
	labels := uiFilterValues(query.Labels, query.Label)
	if len(labels) > 0 {
		// #nosec G202 -- the dynamic fragment contains only count-derived ? placeholders; values stay bound.
		statement += ` AND EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label IN (` +
			uiSQLitePlaceholders(len(labels)) + `))`
		for _, label := range labels {
			args = append(args, label)
		}
	}
	if query.Text != "" {
		statement += ` AND (LOWER(i.title) LIKE ? OR LOWER(i.body) LIKE ? OR LOWER(i.short_id) LIKE ?)`
		needle := "%" + strings.ToLower(query.Text) + "%"
		args = append(args, needle, needle, needle)
	}
	if len(query.Relationships) > 0 {
		predicates := uiSQLiteRelationshipPredicates(query.Relationships)
		// #nosec G202 -- predicates are selected from fixed SQL fragments; relationship values are not interpolated.
		statement += ` AND (` + strings.Join(predicates, " OR ") + `)`
	}
	limit := query.Limit
	if limit > 1000 {
		limit = 1000
	}
	statement += ` ORDER BY i.updated_at DESC, i.id DESC`
	if limit > 0 {
		statement += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	issues := []db.UIIssue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, makeUIIssue(issue, projectNames[issue.ProjectID], nil))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate UI issues: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close UI issues: %w", err)
	}
	if err := readUILabelsForIssues(ctx, tx, issues); err != nil {
		return nil, err
	}
	return issues, nil
}

const uiLabelBatchSize = 500

func readUILabelsForIssues(ctx context.Context, tx *sql.Tx, issues []db.UIIssue) error {
	indexByID := make(map[int64]int, len(issues))
	for idx := range issues {
		issues[idx].Labels = []string{}
		indexByID[issues[idx].ID] = idx
	}
	for start := 0; start < len(issues); start += uiLabelBatchSize {
		end := min(start+uiLabelBatchSize, len(issues))
		args := make([]any, 0, end-start)
		for idx := start; idx < end; idx++ {
			args = append(args, issues[idx].ID)
		}
		// #nosec G202 -- the dynamic fragment contains only count-derived ? placeholders; values stay bound.
		statement := `SELECT issue_id, label FROM issue_labels WHERE issue_id IN (` +
			uiSQLitePlaceholders(end-start) + `) ORDER BY issue_id, label`
		rows, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			return fmt.Errorf("read UI label batch: %w", err)
		}
		for rows.Next() {
			var issueID int64
			var label string
			if err := rows.Scan(&issueID, &label); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan UI label batch: %w", err)
			}
			idx, ok := indexByID[issueID]
			if ok {
				issues[idx].Labels = append(issues[idx].Labels, label)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate UI label batch: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close UI label batch: %w", err)
		}
	}
	return nil
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

func uiSQLitePlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func uiSQLiteRelationshipPredicates(relationships []string) []string {
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

func readUIIssueByUID(
	ctx context.Context, tx *sql.Tx, issueUID string, projectNames map[int64]string,
) (db.UIIssue, error) {
	issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+
		` WHERE i.uid = ? AND i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> ?`,
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
	var archived int
	err := tx.QueryRowContext(ctx, `
		SELECT CASE WHEN i.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL THEN 1 ELSE 0 END
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.uid = ?`, issueUID).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return "missing", nil
	}
	if err != nil {
		return "", fmt.Errorf("read selected UI state: %w", err)
	}
	if archived == 1 {
		return "archived", nil
	}
	return "missing", nil
}

func makeUIIssue(issue db.Issue, projectName string, labels []string) db.UIIssue {
	if labels == nil {
		labels = []string{}
	}
	return db.UIIssue{
		Issue: issue, ProjectName: projectName,
		QualifiedID: projectName + "#" + issue.ShortID, Labels: labels,
	}
}

func readUILabelStrings(ctx context.Context, tx *sql.Tx, issueID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT label FROM issue_labels WHERE issue_id = ? ORDER BY label`, issueID)
	if err != nil {
		return nil, fmt.Errorf("read UI labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	labels := []string{}
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("scan UI label: %w", err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func readUIComments(ctx context.Context, tx *sql.Tx, issueID int64) ([]db.Comment, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, uid, issue_id, author, body, created_at
		FROM comments WHERE issue_id = ? ORDER BY created_at ASC, id ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("read UI comments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	comments := []db.Comment{}
	for rows.Next() {
		var comment db.Comment
		if err := rows.Scan(&comment.ID, &comment.UID, &comment.IssueID, &comment.Author,
			&comment.Body, &comment.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan UI comment: %w", err)
		}
		comments = append(comments, comment)
	}
	return comments, rows.Err()
}

func readUIIssueLabels(ctx context.Context, tx *sql.Tx, issueID int64) ([]db.IssueLabel, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT issue_id, label, author, created_at
		FROM issue_labels WHERE issue_id = ? ORDER BY label`, issueID)
	if err != nil {
		return nil, fmt.Errorf("read selected UI labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	labels := []db.IssueLabel{}
	for rows.Next() {
		var label db.IssueLabel
		if err := rows.Scan(&label.IssueID, &label.Label, &label.Author, &label.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan selected UI label: %w", err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func readUILinksForIssue(ctx context.Context, tx *sql.Tx, issueID int64) ([]db.UILink, error) {
	rows, err := tx.QueryContext(ctx, linkSelect+
		` WHERE (from_issue_id = ? OR to_issue_id = ?)
		AND from_issue_id IN (SELECT endpoint.id FROM issues endpoint JOIN projects endpoint_project ON endpoint_project.id = endpoint.project_id WHERE endpoint.deleted_at IS NULL AND endpoint_project.deleted_at IS NULL)
		AND to_issue_id IN (SELECT endpoint.id FROM issues endpoint JOIN projects endpoint_project ON endpoint_project.id = endpoint.project_id WHERE endpoint.deleted_at IS NULL AND endpoint_project.deleted_at IS NULL)
		ORDER BY id`, issueID, issueID)
	if err != nil {
		return nil, fmt.Errorf("read selected UI links: %w", err)
	}
	links, err := collectUILinks(rows)
	if err != nil {
		return nil, err
	}
	return enrichUILinks(ctx, tx, links)
}

func enrichUILink(ctx context.Context, tx *sql.Tx, link db.Link) (db.UILink, error) {
	var fromProject, fromShort, fromStatus, toProject, toShort, toStatus string
	if err := tx.QueryRowContext(ctx, `
		SELECT fp.name, fi.short_id, fi.status, tp.name, ti.short_id, ti.status
		FROM issues fi JOIN projects fp ON fp.id = fi.project_id
		JOIN issues ti ON ti.id = ? JOIN projects tp ON tp.id = ti.project_id
		WHERE fi.id = ?`, link.ToIssueID, link.FromIssueID).Scan(
		&fromProject, &fromShort, &fromStatus, &toProject, &toShort, &toStatus); err != nil {
		return db.UILink{}, fmt.Errorf("enrich UI link: %w", err)
	}
	return db.UILink{
		Link: link, FromQualifiedID: fromProject + "#" + fromShort, FromStatus: fromStatus,
		ToQualifiedID: toProject + "#" + toShort, ToStatus: toStatus,
	}, nil
}

func readUIRecurrences(ctx context.Context, tx *sql.Tx, projectUID string) ([]db.Recurrence, error) {
	statement := "SELECT " + recurrenceSelectFieldsAliased +
		` FROM recurrences r JOIN projects p ON p.id = r.project_id
		 WHERE r.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> ?`
	args := []any{db.SystemProjectName}
	if projectUID != "" {
		statement += ` AND p.uid = ?`
		args = append(args, projectUID)
	}
	statement += ` ORDER BY r.created_at DESC, r.id DESC`
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI recurrences: %w", err)
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
	return recurrences, rows.Err()
}

func readUIHistory(ctx context.Context, tx *sql.Tx, issueUID string) ([]db.Event, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.uid, e.origin_instance_uid, e.project_id, p.uid, e.project_name,
		e.issue_id, e.issue_uid, i.short_id, e.related_issue_id, e.related_issue_uid, ri.short_id,
		e.type, e.actor, e.payload, e.hlc_physical_ms, e.hlc_counter, e.content_hash, e.created_at
		FROM events e
		JOIN projects p ON p.id = e.project_id
		LEFT JOIN issues i ON i.id = e.issue_id OR (e.issue_id IS NULL AND e.issue_uid IS NOT NULL AND i.uid = e.issue_uid)
		LEFT JOIN issues ri ON ri.id = e.related_issue_id OR (e.related_issue_id IS NULL AND e.related_issue_uid IS NOT NULL AND ri.uid = e.related_issue_uid)
		WHERE e.issue_uid = ? OR e.related_issue_uid = ? ORDER BY e.id DESC LIMIT 500`, issueUID, issueUID)
	if err != nil {
		return nil, fmt.Errorf("read UI history: %w", err)
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
	return history, rows.Err()
}

func readUIGraphLinks(ctx context.Context, tx *sql.Tx, issues []db.UIIssue) ([]db.UILink, error) {
	if len(issues) == 0 {
		return []db.UILink{}, nil
	}
	ids := make([]any, 0, len(issues))
	placeholders := make([]string, len(issues))
	for idx, issue := range issues {
		ids = append(ids, issue.ID)
		placeholders[idx] = "?"
	}
	args := append(append([]any{}, ids...), ids...)
	rows, err := tx.QueryContext(ctx, linkSelect+` WHERE from_issue_id IN (`+
		strings.Join(placeholders, ",")+`) AND to_issue_id IN (`+
		strings.Join(placeholders, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI graph links: %w", err)
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
	placeholders := make([]string, len(issues))
	for idx, issue := range issues {
		visible[issue.ID] = struct{}{}
		args = append(args, issue.ID)
		placeholders[idx] = "?"
	}
	args = append(args, args...)
	rows, err := tx.QueryContext(ctx, linkSelect+` WHERE from_issue_id IN (`+
		strings.Join(placeholders, ",")+`) OR to_issue_id IN (`+
		strings.Join(placeholders, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("read unresolved UI graph links: %w", err)
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
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM issues WHERE id = ?`, missingID).Scan(&exists); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("check unresolved UI graph endpoint: %w", err)
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
	ids := make([]any, 0, len(issues))
	placeholders := make([]string, len(issues))
	for idx, issue := range issues {
		ids = append(ids, issue.ID)
		placeholders[idx] = "?"
	}
	args := append(append([]any{}, ids...), ids...)
	if onDetailRead != nil {
		onDetailRead()
	}
	rows, err := tx.QueryContext(ctx, uiLinkSelect+` WHERE (l.from_issue_id IN (`+
		strings.Join(placeholders, ",")+`) OR l.to_issue_id IN (`+
		strings.Join(placeholders, ",")+`))
		AND fi.deleted_at IS NULL AND fp.deleted_at IS NULL
		AND ti.deleted_at IS NULL AND tp.deleted_at IS NULL
		ORDER BY l.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI collection links: %w", err)
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
		if err := rows.Scan(
			&link.ID, &link.FromIssueID, &link.FromIssueUID,
			&link.ToIssueID, &link.ToIssueUID, &link.Type, &link.Author, &link.CreatedAt,
			&fromProject, &fromShort, &link.FromStatus, &toProject, &toShort, &link.ToStatus,
		); err != nil {
			return nil, fmt.Errorf("scan detailed UI link: %w", err)
		}
		link.FromQualifiedID = fromProject + "#" + fromShort
		link.ToQualifiedID = toProject + "#" + toShort
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return links, rows.Close()
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
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close UI links: %w", err)
	}
	return links, nil
}

func enrichUILinks(ctx context.Context, tx *sql.Tx, links []db.Link) ([]db.UILink, error) {
	enriched := make([]db.UILink, 0, len(links))
	for _, link := range links {
		uiLink, err := enrichUILink(ctx, tx, link)
		if err != nil {
			return nil, err
		}
		enriched = append(enriched, uiLink)
	}
	return enriched, nil
}

func readUIReferenceIssues(
	ctx context.Context, tx *sql.Tx, query db.UIReferencesQuery, limit int,
) ([]db.UIIssueReference, error) {
	statement := `SELECT i.uid, p.uid, p.name, i.short_id, i.title, i.status
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND p.name <> ?`
	args := []any{db.SystemProjectName}
	if query.ProjectUID != "" {
		statement += ` AND p.uid = ?`
		args = append(args, query.ProjectUID)
	}
	if query.Query != "" {
		needle := "%" + strings.ToLower(query.Query) + "%"
		statement += ` AND (LOWER(i.title) LIKE ? OR LOWER(i.short_id) LIKE ? OR LOWER(p.name) LIKE ? OR LOWER(p.name || '#' || i.short_id) LIKE ?)`
		args = append(args, needle, needle, needle, needle)
	}
	statement += ` ORDER BY p.name, i.short_id LIMIT ?`
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI issue references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	issues := []db.UIIssueReference{}
	for rows.Next() {
		var issue db.UIIssueReference
		if err := rows.Scan(&issue.UID, &issue.ProjectUID, &issue.ProjectName,
			&issue.ShortID, &issue.Title, &issue.Status); err != nil {
			return nil, fmt.Errorf("scan UI issue reference: %w", err)
		}
		issue.QualifiedID = issue.ProjectName + "#" + issue.ShortID
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

func readUIReferenceStrings(
	ctx context.Context, tx *sql.Tx, statement string, args ...any,
) ([]string, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read UI reference choices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scan UI reference choice: %w", err)
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
