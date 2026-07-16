package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/shortid"
	katauid "go.kenn.io/kata/internal/uid"
)

const issueSelect = `SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision, i.recurrence_id,
       i.occurrence_key, i.created_at, i.updated_at, i.closed_at, i.deleted_at
  FROM issues i JOIN projects p ON p.id = i.project_id`

type issueCreatedPayload struct {
	UID                    string                 `json:"uid"`
	ShortID                string                 `json:"short_id"`
	Title                  string                 `json:"title"`
	Body                   string                 `json:"body"`
	Author                 string                 `json:"author"`
	Owner                  *string                `json:"owner,omitempty"`
	Priority               *int64                 `json:"priority,omitempty"`
	Status                 string                 `json:"status"`
	ClosedReason           *string                `json:"closed_reason,omitempty"`
	ClosedAt               *string                `json:"closed_at,omitempty"`
	DeletedAt              *string                `json:"deleted_at,omitempty"`
	Metadata               json.RawMessage        `json:"metadata"`
	Labels                 []string               `json:"labels,omitempty"`
	Links                  []createdLink          `json:"links,omitempty"`
	Comments               []issueSnapshotComment `json:"comments,omitempty"`
	CreatedAt              string                 `json:"created_at"`
	UpdatedAt              string                 `json:"updated_at,omitempty"`
	Revision               int64                  `json:"revision,omitempty"`
	IdempotencyKey         string                 `json:"idempotency_key,omitempty"`
	IdempotencyFingerprint string                 `json:"idempotency_fingerprint,omitempty"`
	RecurrenceUID          string                 `json:"recurrence_uid,omitempty"`
	OccurrenceKey          string                 `json:"occurrence_key,omitempty"`
	Source                 string                 `json:"source,omitempty"`
	ExternalID             string                 `json:"external_id,omitempty"`
}

type createdLink struct {
	Type       string `json:"type"`
	ToShortID  string `json:"to_short_id,omitempty"`
	ToIssueUID string `json:"to_issue_uid,omitempty"`
	Incoming   bool   `json:"incoming,omitempty"`
	Author     string `json:"author,omitempty"`
}

type issueSnapshotComment struct {
	CommentUID string `json:"comment_uid"`
	Author     string `json:"author"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
}

// CreateIssue persists the initial issue projection and its creation event in
// one serializable transaction.
func (s *Store) CreateIssue(ctx context.Context, params db.CreateIssueParams) (db.Issue, db.Event, error) {
	metadataBlob, err := composeCreateMetadata(params.Metadata)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	links, err := normalizeInitialLinks(params.Links)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	labels := dedupeAndSort(params.Labels)
	if err := validateInitialLabels(labels); err != nil {
		return db.Issue{}, db.Event{}, err
	}
	owner := params.Owner
	if owner != nil && *owner == "" {
		owner = nil
	}

	var issue db.Issue
	var event db.Event
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var project db.Project
		project, err = scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id = $1 AND deleted_at IS NULL FOR SHARE`, params.ProjectID))
		if err != nil {
			return err
		}
		if err := ensureProjectWritableTx(ctx, tx, project.ID); err != nil {
			return err
		}
		effectiveActor, err := effectiveLocalMutationActorTx(ctx, tx, project.ID, params.Author)
		if err != nil {
			return err
		}
		// Short IDs are allocated from a project-local namespace. Serialize
		// allocation within that namespace so two distinct UIDs sharing a
		// suffix observe one another and the later writer extends its suffix.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, params.ProjectID); err != nil {
			return mapSQLError(err, nil)
		}
		issueUID := params.UID
		if issueUID == "" {
			issueUID, err = katauid.New()
			if err != nil {
				return fmt.Errorf("generate issue uid: %w", err)
			}
		} else if !katauid.Valid(issueUID) {
			return fmt.Errorf("invalid issue uid %q", issueUID)
		}
		shortIDValue, err := s.resolveShortIDTx(ctx, tx, params.ProjectID, issueUID, params.ShortIDOverride)
		if err != nil {
			return err
		}
		createdAt := nowStoredTimestamp()
		var issueID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO issues(
          uid, project_id, short_id, title, body, author, owner, priority, metadata, created_at, updated_at
        ) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10) RETURNING id`,
			issueUID, params.ProjectID, shortIDValue, params.Title, params.Body, effectiveActor,
			owner, params.Priority, string(metadataBlob), createdAt,
		).Scan(&issueID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		for _, label := range labels {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO issue_labels(issue_id, label, author) VALUES($1,$2,$3)`, issueID, label, effectiveActor); err != nil {
				return mapSQLError(err, nil)
			}
		}
		linkPayloads := make([]createdLink, 0, len(links))
		for _, link := range links {
			var targetID int64
			var targetUID, targetShortID string
			err := tx.QueryRowContext(ctx,
				`SELECT id, uid, short_id FROM issues WHERE id = $1 AND deleted_at IS NULL`, link.ToNumber,
			).Scan(&targetID, &targetUID, &targetShortID)
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrInitialLinkTargetNotFound
			}
			if err != nil {
				return mapSQLError(err, nil)
			}
			if targetID == issueID {
				return db.ErrSelfLink
			}
			fromID, toID := issueID, targetID
			fromUID, toUID := issueUID, targetUID
			if link.Incoming && link.Type == "blocks" {
				fromID, toID = targetID, issueID
				fromUID, toUID = targetUID, issueUID
			}
			if link.Type == "related" && fromID > toID {
				fromID, toID = toID, fromID
				fromUID, toUID = toUID, fromUID
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO links(
              from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author
            ) VALUES($1,$2,$3,$4,$5,$6)`,
				fromID, toID, fromUID, toUID, link.Type, effectiveActor); err != nil {
				return mapSQLError(err, linkConstraintErrors)
			}
			linkPayloads = append(linkPayloads, createdLink{
				Type: link.Type, ToShortID: targetShortID, ToIssueUID: targetUID,
				Incoming: link.Incoming, Author: effectiveActor,
			})
		}
		payload, err := json.Marshal(issueCreatedPayload{
			UID: issueUID, ShortID: shortIDValue, Title: params.Title, Body: params.Body,
			Author: effectiveActor, Owner: owner, Priority: params.Priority, Status: "open",
			Metadata: metadataBlob, Labels: labels, Links: linkPayloads, CreatedAt: createdAt,
			IdempotencyKey: params.IdempotencyKey, IdempotencyFingerprint: params.IdempotencyFingerprint,
		})
		if err != nil {
			return fmt.Errorf("marshal issue.created payload: %w", err)
		}
		event, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: params.ProjectID, ProjectUID: project.UID, ProjectName: project.Name,
			IssueID: &issueID, IssueUID: &issueUID, Type: "issue.created", Actor: effectiveActor,
			Payload: string(payload),
		})
		if err != nil {
			return err
		}
		issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, issueID))
		return err
	})
	return issue, event, err
}

// IssueByID returns an issue regardless of soft-deletion state.
func (s *Store) IssueByID(ctx context.Context, id int64) (db.Issue, error) {
	return scanIssue(s.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, id))
}

// IssueByShortID resolves a project-local display identifier.
func (s *Store) IssueByShortID(ctx context.Context, projectID int64, value string, include db.IncludeDeleted) (db.Issue, error) {
	query := issueSelect + ` WHERE i.project_id = $1 AND i.short_id = $2`
	if include == db.IncludeDeletedNo {
		query += ` AND i.deleted_at IS NULL`
	}
	return scanIssue(s.QueryRowContext(ctx, query, projectID, value))
}

// IssueByUID resolves a stable issue identifier.
func (s *Store) IssueByUID(ctx context.Context, uid string, include db.IncludeDeleted) (db.Issue, error) {
	query := issueSelect + ` WHERE i.uid = $1`
	if include == db.IncludeDeletedNo {
		query += ` AND i.deleted_at IS NULL`
	}
	return scanIssue(s.QueryRowContext(ctx, query, uid))
}

// IssueUIDPrefixMatch returns deterministic UID-prefix matches.
func (s *Store) IssueUIDPrefixMatch(
	ctx context.Context,
	prefix string,
	limit int,
	include db.IncludeDeleted,
) ([]db.Issue, error) {
	if limit <= 0 {
		limit = 20
	}
	query := issueSelect + ` WHERE i.uid LIKE $1 || '%'`
	if include == db.IncludeDeletedNo {
		query += ` AND i.deleted_at IS NULL`
	}
	query += ` ORDER BY i.uid ASC LIMIT $2`
	rows, err := s.QueryContext(ctx, query, prefix, limit)
	if err != nil {
		return nil, mapSQLError(err, nil)
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
	return issues, mapSQLError(rows.Err(), nil)
}

// ListIssues returns active issues matching the project filters.
func (s *Store) ListIssues(ctx context.Context, params db.ListIssuesParams) ([]db.Issue, error) {
	conditions := []string{"i.project_id = $1", "i.deleted_at IS NULL"}
	args := []any{params.ProjectID}
	add := func(predicate string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(predicate, len(args)))
	}
	if params.Status != "" {
		add("i.status = $%d", params.Status)
	}
	if params.Priority != nil {
		add("i.priority = $%d", *params.Priority)
	}
	if params.MaxPriority != nil {
		add("i.priority IS NOT NULL AND i.priority <= $%d", *params.MaxPriority)
	}
	if params.Unowned {
		conditions = append(conditions, "i.owner IS NULL")
	} else if params.Owner != "" {
		add("i.owner = $%d", params.Owner)
	}
	for _, label := range params.Labels {
		add("EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = $%d)", strings.ToLower(label))
	}
	for _, label := range params.ExcludeLabels {
		add("NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id AND il.label = $%d)", strings.ToLower(label))
	}
	for _, filter := range params.Meta {
		args = append(args, filter.Key)
		keyPosition := len(args)
		if filter.HasValue {
			args = append(args, filter.Value)
			conditions = append(conditions, fmt.Sprintf(
				`EXISTS (SELECT 1 FROM jsonb_each(i.metadata::jsonb) entry
                  WHERE entry.key = $%d AND jsonb_typeof(entry.value) = 'string'
                    AND entry.value #>> '{}' = $%d)`,
				keyPosition, len(args)))
		} else {
			conditions = append(conditions, fmt.Sprintf(`i.metadata::jsonb ? $%d`, keyPosition))
		}
	}
	query := issueSelect + ` WHERE ` + strings.Join(conditions, " AND ") + ` ORDER BY i.updated_at DESC, i.id DESC`
	if params.Limit > 0 {
		args = append(args, params.Limit)
		query += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
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
		return nil, mapSQLError(err, nil)
	}
	return issues, nil
}

// ListAllIssues returns a newest-first cross-project issue page.
func (s *Store) ListAllIssues(ctx context.Context, params db.ListAllIssuesParams) ([]db.Issue, error) {
	conditions := []string{"i.deleted_at IS NULL", "p.deleted_at IS NULL"}
	var args []any
	add := func(predicate string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(predicate, len(args)))
	}
	if params.ProjectID > 0 {
		add("i.project_id = $%d", params.ProjectID)
	}
	if params.Status != "" {
		add("i.status = $%d", params.Status)
	}
	if params.Priority != nil {
		add("i.priority = $%d", *params.Priority)
	}
	if params.MaxPriority != nil {
		add("i.priority IS NOT NULL AND i.priority <= $%d", *params.MaxPriority)
	}
	query := issueSelect + ` WHERE ` + strings.Join(conditions, " AND ") + ` ORDER BY i.created_at DESC, i.id DESC`
	if params.Limit > 0 {
		args = append(args, params.Limit)
		query += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
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
	return issues, mapSQLError(rows.Err(), nil)
}

func (s *Store) resolveShortIDTx(ctx context.Context, tx *sql.Tx, projectID int64, uid, override string) (string, error) {
	if override != "" {
		if !shortid.Valid(override) {
			return "", fmt.Errorf("invalid short_id override %q", override)
		}
		expected, err := shortid.Derive(uid, len(override))
		if err != nil || expected != override {
			return "", fmt.Errorf("short_id override %q does not match uid", override)
		}
		return override, nil
	}
	for length := shortid.MinLength; length <= shortid.MaxLength; length++ {
		candidate, err := shortid.Derive(uid, length)
		if err != nil {
			return "", err
		}
		var exists bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(
          SELECT 1 FROM issues WHERE project_id = $1 AND short_id = $2 AND uid <> $3
          UNION ALL SELECT 1 FROM purge_log WHERE project_id = $1 AND short_id = $2
        )`, projectID, candidate, uid).Scan(&exists)
		if err != nil {
			return "", mapSQLError(err, nil)
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("short_id auto-extend exhausted for uid %s", uid)
}

func composeCreateMetadata(input map[string]json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		return json.RawMessage(`{}`), nil
	}
	for key, value := range input {
		if err := metadata.ValidateCreateValue(metadata.IssueRegistry, key, value); err != nil {
			return nil, fmt.Errorf("metadata %q: %w", key, err)
		}
	}
	value, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal issue metadata: %w", err)
	}
	return value, nil
}

func normalizeInitialLinks(input []db.InitialLink) ([]db.InitialLink, error) {
	type key struct {
		Type string
		ID   int64
		In   bool
	}
	seen := make(map[key]struct{}, len(input))
	links := make([]db.InitialLink, 0, len(input))
	parentCount := 0
	for _, link := range input {
		if link.Type != "parent" && link.Type != "blocks" && link.Type != "related" {
			return nil, db.ErrInitialLinkInvalidType
		}
		if link.Type == "parent" && link.Incoming {
			return nil, db.ErrInitialLinkInvalidType
		}
		if link.Type == "related" {
			link.Incoming = false
		}
		identity := key{Type: link.Type, ID: link.ToNumber, In: link.Incoming}
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		if link.Type == "parent" {
			parentCount++
			if parentCount > 1 {
				return nil, db.ErrParentAlreadySet
			}
		}
		links = append(links, link)
	}
	return links, nil
}

func dedupeAndSort(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	result := make([]string, 0, len(input))
	for _, value := range input {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validateInitialLabels(labels []string) error {
	for _, label := range labels {
		if len(label) < 1 || len(label) > 64 {
			return db.ErrLabelInvalid
		}
		for _, char := range label {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || strings.ContainsRune("._:-", char) {
				continue
			}
			return db.ErrLabelInvalid
		}
	}
	return nil
}

func scanIssue(row rowScanner) (db.Issue, error) {
	var buffer issueScanBuffer
	err := row.Scan(buffer.destinations()...)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Issue{}, db.ErrNotFound
	}
	if err != nil {
		return db.Issue{}, mapSQLError(err, nil)
	}
	return buffer.value()
}

type issueScanBuffer struct {
	issue                              db.Issue
	metadataBlob, createdAt, updatedAt string
	closedAt, deletedAt                sql.NullString
}

func (b *issueScanBuffer) destinations() []any {
	return []any{
		&b.issue.ID, &b.issue.UID, &b.issue.ProjectID, &b.issue.ProjectUID, &b.issue.ShortID,
		&b.issue.Title, &b.issue.Body, &b.issue.Status, &b.issue.ClosedReason, &b.issue.Owner,
		&b.issue.Priority, &b.issue.Author, &b.metadataBlob, &b.issue.Revision, &b.issue.RecurrenceID,
		&b.issue.OccurrenceKey, &b.createdAt, &b.updatedAt, &b.closedAt, &b.deletedAt,
	}
}

func (b *issueScanBuffer) value() (db.Issue, error) {
	var err error
	b.issue.Metadata = db.JSONBlob(b.metadataBlob)
	b.issue.CreatedAt, err = parseStoredTime(b.createdAt)
	if err != nil {
		return db.Issue{}, fmt.Errorf("parse issue created_at: %w", err)
	}
	b.issue.UpdatedAt, err = parseStoredTime(b.updatedAt)
	if err != nil {
		return db.Issue{}, fmt.Errorf("parse issue updated_at: %w", err)
	}
	if b.closedAt.Valid {
		value, err := parseStoredTime(b.closedAt.String)
		if err != nil {
			return db.Issue{}, fmt.Errorf("parse issue closed_at: %w", err)
		}
		b.issue.ClosedAt = &value
	}
	if b.deletedAt.Valid {
		value, err := parseStoredTime(b.deletedAt.String)
		if err != nil {
			return db.Issue{}, fmt.Errorf("parse issue deleted_at: %w", err)
		}
		b.issue.DeletedAt = &value
	}
	return b.issue, nil
}
