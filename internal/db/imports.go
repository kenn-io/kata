package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	katauid "go.kenn.io/kata/internal/uid"
)

// ImportBatchParams is the input to ImportBatch: the project receiving the
// import, the source identifier (e.g. "beads"), the actor recorded on emitted
// events, and the normalized issue items to upsert.
type ImportBatchParams struct {
	ProjectID int64
	Source    string
	Actor     string
	Items     []ImportItem
}

// ImportItem is one normalized issue in an import batch. ExternalID is the
// source-side identifier used for upsert via import_mappings; CreatedAt and
// UpdatedAt drive timestamp fidelity and source-vs-local conflict resolution.
type ImportItem struct {
	ExternalID   string
	Title        string
	Body         string
	Author       string
	Owner        *string
	Priority     *int64
	Status       string
	ClosedReason *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ClosedAt     *time.Time
	Labels       []string
	Comments     []ImportComment
	Links        []ImportLink
}

// ImportComment is one normalized comment attached to an ImportItem. ExternalID
// is the source-side comment identifier used for upsert via import_mappings.
type ImportComment struct {
	ExternalID string
	Author     string
	Body       string
	CreatedAt  time.Time
}

// ImportLink is one normalized outgoing link from an ImportItem. TargetExternalID
// references another item's ExternalID in the same batch (or an existing mapped
// item); the daemon resolves it to a kata issue number.
type ImportLink struct {
	Type             string
	TargetExternalID string
}

// ImportBatchResult summarizes a completed import batch: per-status counts and
// a per-item breakdown the CLI uses for human and JSON output.
type ImportBatchResult struct {
	Source    string             `json:"source"`
	Created   int                `json:"created"`
	Updated   int                `json:"updated"`
	Unchanged int                `json:"unchanged"`
	Comments  int                `json:"comments"`
	Links     int                `json:"links"`
	Items     []ImportItemResult `json:"items"`
	Errors    []string           `json:"errors"`
}

// ImportItemResult is the per-item entry in ImportBatchResult.Items. Status is
// "created", "updated", or "unchanged"; Reason carries an optional rationale
// (e.g. "local newer").
type ImportItemResult struct {
	ExternalID   string `json:"external_id"`
	IssueShortID string `json:"issue_short_id"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
}

// ErrImportValidation is returned by ImportBatch when the request fails
// validation (missing fields, bad status, unresolved link target). The daemon
// translates it into a 400 with kind="import_validation".
var ErrImportValidation = errors.New("invalid import")

type importIssueState struct {
	item        ImportItem
	issue       Issue
	created     bool
	sourceNewer bool
}

// ImportBatch imports external issues atomically. Issues and comments are
// upserted through import_mappings; labels and links managed by this source are
// reconciled only when the source issue version is newer than kata's row (or the
// issue is newly created).
func (d *DB) ImportBatch(ctx context.Context, p ImportBatchParams) (ImportBatchResult, []Event, error) {
	if err := validateImportBatch(p); err != nil {
		return ImportBatchResult{}, nil, err
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return ImportBatchResult{}, nil, fmt.Errorf("begin import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var projectName, projectUID string
	if err := tx.QueryRowContext(ctx,
		`SELECT name, uid FROM projects WHERE id = ? AND deleted_at IS NULL`, p.ProjectID).
		Scan(&projectName, &projectUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ImportBatchResult{}, nil, ErrNotFound
		}
		return ImportBatchResult{}, nil, fmt.Errorf("lookup import project: %w", err)
	}
	if err := ensureProjectWritableTx(ctx, tx, p.ProjectID); err != nil {
		return ImportBatchResult{}, nil, err
	}

	result := ImportBatchResult{Source: p.Source, Items: make([]ImportItemResult, 0, len(p.Items)), Errors: []string{}}
	events := []Event{}
	states := make(map[string]*importIssueState, len(p.Items))

	for _, item := range p.Items {
		state, evt, err := d.importIssue(ctx, tx, p, item, projectName, projectUID)
		if err != nil {
			return ImportBatchResult{}, nil, err
		}
		if evt != nil {
			events = append(events, *evt)
		}
		states[item.ExternalID] = state
		switch {
		case state.created:
			result.Created++
		case state.sourceNewer:
			result.Updated++
		default:
			result.Unchanged++
		}
		status := "unchanged"
		if state.created {
			status = "created"
		} else if state.sourceNewer {
			status = "updated"
		}
		result.Items = append(result.Items, ImportItemResult{ExternalID: item.ExternalID, IssueShortID: state.issue.ShortID, Status: status})
	}

	for _, item := range p.Items {
		state := states[item.ExternalID]
		// Defensive: the first loop populates an entry per item so this
		// lookup always hits, but nilaway can't infer that — skip
		// rather than deref a nil *importIssueState if the invariant
		// ever drifts.
		if state == nil {
			continue
		}
		commentEvents, n, err := d.importComments(ctx, tx, p, state.issue, item, projectName)
		if err != nil {
			return ImportBatchResult{}, nil, err
		}
		events = append(events, commentEvents...)
		result.Comments += n
		if state.created || state.sourceNewer {
			labelEvents, err := d.reconcileImportLabels(ctx, tx, p, state.issue, item, projectName)
			if err != nil {
				return ImportBatchResult{}, nil, err
			}
			events = append(events, labelEvents...)
		}
	}

	for _, item := range p.Items {
		state := states[item.ExternalID]
		if state == nil {
			continue
		}
		if state.created || state.sourceNewer {
			linkEvents, n, err := d.reconcileImportLinks(ctx, tx, p, state.issue, item, states, projectName)
			if err != nil {
				return ImportBatchResult{}, nil, err
			}
			events = append(events, linkEvents...)
			result.Links += n
		}
	}

	if err := tx.Commit(); err != nil {
		return ImportBatchResult{}, nil, fmt.Errorf("commit import: %w", err)
	}
	return result, events, nil
}

func validateImportBatch(p ImportBatchParams) error {
	if strings.TrimSpace(p.Source) == "" || strings.TrimSpace(p.Actor) == "" {
		return fmt.Errorf("%w: source and actor are required", ErrImportValidation)
	}
	seenItems := map[string]struct{}{}
	seenComments := map[string]struct{}{}
	for _, item := range p.Items {
		if strings.TrimSpace(item.ExternalID) == "" || strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.Author) == "" {
			return fmt.Errorf("%w: external_id, title, and author are required", ErrImportValidation)
		}
		if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
			return fmt.Errorf("%w: created_at and updated_at are required", ErrImportValidation)
		}
		if item.UpdatedAt.Before(item.CreatedAt) {
			return fmt.Errorf("%w: updated_at cannot be before created_at", ErrImportValidation)
		}
		if _, ok := seenItems[item.ExternalID]; ok {
			return fmt.Errorf("%w: duplicate item external_id %q", ErrImportValidation, item.ExternalID)
		}
		seenItems[item.ExternalID] = struct{}{}
		if item.Status != "open" && item.Status != "closed" {
			return fmt.Errorf("%w: status must be open or closed", ErrImportValidation)
		}
		if item.ClosedAt != nil && item.ClosedAt.Before(item.CreatedAt) {
			return fmt.Errorf("%w: closed_at cannot be before created_at", ErrImportValidation)
		}
		if item.Status == "open" && (item.ClosedReason != nil || item.ClosedAt != nil) {
			return fmt.Errorf("%w: open issues cannot have closed fields", ErrImportValidation)
		}
		if item.Status == "closed" && item.ClosedAt == nil {
			return fmt.Errorf("%w: closed issues require closed_at", ErrImportValidation)
		}
		if item.ClosedReason != nil && !validImportClosedReason(*item.ClosedReason) {
			return fmt.Errorf("%w: closed_reason must be one of done, wontfix, duplicate, superseded, audit-no-change", ErrImportValidation)
		}
		if item.Priority != nil && (*item.Priority < 0 || *item.Priority > 4) {
			return fmt.Errorf("%w: priority must be between 0 and 4", ErrImportValidation)
		}
		for _, label := range item.Labels {
			if !validImportLabel(label) {
				return fmt.Errorf("%w: invalid label %q", ErrImportValidation, label)
			}
		}
		for _, c := range item.Comments {
			if strings.TrimSpace(c.ExternalID) == "" || strings.TrimSpace(c.Author) == "" || strings.TrimSpace(c.Body) == "" || c.CreatedAt.IsZero() {
				return fmt.Errorf("%w: comment external_id, author, body, and created_at are required", ErrImportValidation)
			}
			if _, ok := seenComments[c.ExternalID]; ok {
				return fmt.Errorf("%w: duplicate comment external_id %q", ErrImportValidation, c.ExternalID)
			}
			seenComments[c.ExternalID] = struct{}{}
		}
		for _, l := range item.Links {
			if l.Type != "blocks" && l.Type != "parent" && l.Type != "related" {
				return fmt.Errorf("%w: link type must be parent|blocks|related", ErrImportValidation)
			}
			if strings.TrimSpace(l.TargetExternalID) == "" {
				return fmt.Errorf("%w: link target_external_id is required", ErrImportValidation)
			}
		}
	}
	return nil
}

func (d *DB) importIssue(ctx context.Context, tx *sql.Tx, p ImportBatchParams, item ImportItem, projectName, projectUID string) (*importIssueState, *Event, error) {
	mapping, err := importMappingBySource(ctx, tx, p.ProjectID, p.Source, "issue", item.ExternalID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	if errors.Is(err, ErrNotFound) {
		issue, evt, err := d.insertImportedIssue(ctx, tx, p, item, projectName, projectUID)
		if err != nil {
			return nil, nil, err
		}
		_, err = upsertImportMapping(ctx, tx, ImportMappingParams{Source: p.Source, ExternalID: item.ExternalID, ObjectType: "issue", ProjectID: p.ProjectID, IssueID: &issue.ID, SourceUpdatedAt: &item.UpdatedAt})
		if err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: issue, created: true, sourceNewer: true}, &evt, nil
	}
	if mapping.IssueID == nil {
		return nil, nil, fmt.Errorf("%w: issue mapping missing issue_id", ErrNotFound)
	}
	existing, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ? AND i.deleted_at IS NULL`, *mapping.IssueID))
	if err != nil {
		return nil, nil, err
	}
	if item.UpdatedAt.After(existing.UpdatedAt) {
		updated, evt, err := d.updateImportedIssue(ctx, tx, p, item, existing, projectName)
		if err != nil {
			return nil, nil, err
		}
		_, err = upsertImportMapping(ctx, tx, ImportMappingParams{Source: p.Source, ExternalID: item.ExternalID, ObjectType: "issue", ProjectID: p.ProjectID, IssueID: &updated.ID, SourceUpdatedAt: &item.UpdatedAt})
		if err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: updated, sourceNewer: true}, &evt, nil
	}
	_, err = upsertImportMapping(ctx, tx, ImportMappingParams{Source: p.Source, ExternalID: item.ExternalID, ObjectType: "issue", ProjectID: p.ProjectID, IssueID: &existing.ID, SourceUpdatedAt: &item.UpdatedAt})
	if err != nil {
		return nil, nil, err
	}
	return &importIssueState{item: item, issue: existing}, nil, nil
}

func (d *DB) insertImportedIssue(ctx context.Context, tx *sql.Tx, p ImportBatchParams, item ImportItem, projectName, projectUID string) (Issue, Event, error) {
	// Validate project exists and is not archived.
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE id = ? AND deleted_at IS NULL`, p.ProjectID).
		Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Issue{}, Event{}, ErrNotFound
		}
		return Issue{}, Event{}, fmt.Errorf("check project: %w", err)
	}
	issueUID, err := katauid.New()
	if err != nil {
		return Issue{}, Event{}, fmt.Errorf("generate issue uid: %w", err)
	}
	shortID, err := assignShortID(ctx, tx, p.ProjectID, issueUID)
	if err != nil {
		return Issue{}, Event{}, fmt.Errorf("assign import short_id: %w", err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO issues(uid, project_id, short_id, title, body, status, closed_reason, owner, author, created_at, updated_at, closed_at, priority)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		issueUID, p.ProjectID, shortID, item.Title, item.Body, item.Status, item.ClosedReason, normalizeOwner(item.Owner), item.Author, item.CreatedAt, item.UpdatedAt, item.ClosedAt, item.Priority)
	if err != nil {
		return Issue{}, Event{}, fmt.Errorf("insert imported issue: %w", err)
	}
	issueID, err := res.LastInsertId()
	if err != nil {
		return Issue{}, Event{}, fmt.Errorf("last issue id: %w", err)
	}
	payload, err := buildIssueCreatedPayload(issueCreatedPayload{
		UID:          issueUID,
		ShortID:      shortID,
		Title:        item.Title,
		Body:         item.Body,
		Author:       item.Author,
		Owner:        normalizeOwner(item.Owner),
		Priority:     item.Priority,
		Status:       item.Status,
		ClosedReason: item.ClosedReason,
		ClosedAt:     formatOptionalSQLiteTime(item.ClosedAt),
		Metadata:     json.RawMessage(`{}`),
		CreatedAt:    item.CreatedAt.UTC().Format(sqliteTimeFormat),
		UpdatedAt:    item.UpdatedAt.UTC().Format(sqliteTimeFormat),
		Source:       p.Source,
		ExternalID:   item.ExternalID,
	})
	if err != nil {
		return Issue{}, Event{}, err
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectUID: projectUID, ProjectName: projectName, IssueID: &issueID, IssueUID: &issueUID, Type: "issue.created", Actor: p.Actor, Payload: payload})
	if err != nil {
		return Issue{}, Event{}, err
	}
	issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, issueID))
	if err != nil {
		return Issue{}, Event{}, err
	}
	return issue, evt, nil
}

func (d *DB) updateImportedIssue(ctx context.Context, tx *sql.Tx, p ImportBatchParams, item ImportItem, existing Issue, projectName string) (Issue, Event, error) {
	_, err := tx.ExecContext(ctx, `UPDATE issues
		SET title = ?, body = ?, status = ?, closed_reason = ?, owner = ?, updated_at = ?, closed_at = ?, priority = ?
		WHERE id = ?`, item.Title, item.Body, item.Status, item.ClosedReason, normalizeOwner(item.Owner), item.UpdatedAt, item.ClosedAt, item.Priority, existing.ID)
	if err != nil {
		return Issue{}, Event{}, fmt.Errorf("update imported issue: %w", err)
	}
	payload, err := importedIssueUpdatedPayload(p.Source, item.ExternalID, existing, item)
	if err != nil {
		return Issue{}, Event{}, err
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectName: projectName, IssueID: &existing.ID, Type: "issue.updated", Actor: p.Actor, Payload: payload})
	if err != nil {
		return Issue{}, Event{}, err
	}
	updated, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, existing.ID))
	if err != nil {
		return Issue{}, Event{}, err
	}
	return updated, evt, nil
}

func (d *DB) importComments(ctx context.Context, tx *sql.Tx, p ImportBatchParams, issue Issue, item ImportItem, projectName string) ([]Event, int, error) {
	events := []Event{}
	created := 0
	for _, c := range item.Comments {
		mapping, err := importMappingBySource(ctx, tx, p.ProjectID, p.Source, "comment", c.ExternalID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, 0, err
		}
		if err == nil {
			if mapping.IssueID != nil && *mapping.IssueID != issue.ID {
				return nil, 0, fmt.Errorf("%w: comment %q is mapped to a different issue", ErrImportValidation, c.ExternalID)
			}
			continue
		}
		commentUID, err := katauid.New()
		if err != nil {
			return nil, 0, fmt.Errorf("generate imported comment uid: %w", err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO comments(uid, issue_id, author, body, created_at) VALUES(?, ?, ?, ?, ?)`, commentUID, issue.ID, c.Author, c.Body, c.CreatedAt)
		if err != nil {
			return nil, 0, fmt.Errorf("insert imported comment: %w", err)
		}
		commentID, err := res.LastInsertId()
		if err != nil {
			return nil, 0, fmt.Errorf("last comment id: %w", err)
		}
		_, err = upsertImportMapping(ctx, tx, ImportMappingParams{Source: p.Source, ExternalID: c.ExternalID, ObjectType: "comment", ProjectID: p.ProjectID, IssueID: &issue.ID, CommentID: &commentID})
		if err != nil {
			return nil, 0, err
		}
		payload, err := json.Marshal(map[string]any{
			"comment_uid":         commentUID,
			"author":              c.Author,
			"body":                c.Body,
			"created_at":          c.CreatedAt.UTC().Format(sqliteTimeFormat),
			"source":              p.Source,
			"external_id":         item.ExternalID,
			"comment_external_id": c.ExternalID,
		})
		if err != nil {
			return nil, 0, fmt.Errorf("marshal import comment payload: %w", err)
		}
		evt, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectName: projectName, IssueID: &issue.ID, Type: "issue.commented", Actor: p.Actor, Payload: string(payload)})
		if err != nil {
			return nil, 0, err
		}
		events = append(events, evt)
		created++
	}
	return events, created, nil
}

func (d *DB) reconcileImportLabels(ctx context.Context, tx *sql.Tx, p ImportBatchParams, issue Issue, item ImportItem, projectName string) ([]Event, error) {
	events := []Event{}
	desired := map[string]string{}
	for _, label := range dedupeStrings(item.Labels) {
		desired[label] = importLabelExternalID(item.ExternalID, label)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, external_id, label FROM import_mappings WHERE project_id = ? AND source = ? AND object_type = 'label' AND issue_id = ?`, p.ProjectID, p.Source, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("list source labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	existingMappings := map[string]int64{}
	for rows.Next() {
		var id int64
		var externalID string
		var label sql.NullString
		if err := rows.Scan(&id, &externalID, &label); err != nil {
			return nil, fmt.Errorf("scan source label mapping: %w", err)
		}
		if label.Valid {
			existingMappings[label.String] = id
		}
		if !label.Valid || desired[label.String] != externalID {
			if label.Valid {
				if _, err := tx.ExecContext(ctx, `DELETE FROM issue_labels WHERE issue_id = ? AND label = ?`, issue.ID, label.String); err != nil {
					return nil, fmt.Errorf("delete source label: %w", err)
				}
				evt, err := d.insertLabelEvent(ctx, tx, p, issue, projectName, item.ExternalID, "issue.unlabeled", label.String, item.UpdatedAt)
				if err != nil {
					return nil, err
				}
				events = append(events, evt)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings WHERE id = ?`, id); err != nil {
				return nil, fmt.Errorf("delete source label mapping: %w", err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for label, externalID := range desired {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO issue_labels(issue_id, label, author, created_at) VALUES(?, ?, ?, ?)`, issue.ID, label, p.Actor, item.CreatedAt)
		if err != nil {
			return nil, classifyLabelInsertError(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("label rows affected: %w", err)
		}
		if _, ok := existingMappings[label]; ok || affected > 0 {
			_, err = upsertImportMapping(ctx, tx, ImportMappingParams{Source: p.Source, ExternalID: externalID, ObjectType: "label", ProjectID: p.ProjectID, IssueID: &issue.ID, Label: &label, SourceUpdatedAt: &item.UpdatedAt})
			if err != nil {
				return nil, err
			}
		}
		if affected > 0 {
			evt, err := d.insertLabelEvent(ctx, tx, p, issue, projectName, item.ExternalID, "issue.labeled", label, item.UpdatedAt)
			if err != nil {
				return nil, err
			}
			events = append(events, evt)
		}
	}
	return events, nil
}

func (d *DB) insertLabelEvent(ctx context.Context, tx *sql.Tx, p ImportBatchParams, issue Issue, projectName, itemExternalID, eventType, label string, updatedAt time.Time) (Event, error) {
	payload, err := json.Marshal(map[string]any{
		"issue_uid":   issue.UID,
		"source":      p.Source,
		"external_id": importLabelExternalID(itemExternalID, label),
		"label":       label,
		"updated_at":  updatedAt.UTC().Format(sqliteTimeFormat),
	})
	if err != nil {
		return Event{}, fmt.Errorf("marshal label payload: %w", err)
	}
	return d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectName: projectName, IssueID: &issue.ID, Type: eventType, Actor: p.Actor, Payload: string(payload)})
}

func (d *DB) reconcileImportLinks(ctx context.Context, tx *sql.Tx, p ImportBatchParams, issue Issue, item ImportItem, states map[string]*importIssueState, projectName string) ([]Event, int, error) {
	events := []Event{}
	created := 0
	desired := map[string]ImportLink{}
	for _, l := range item.Links {
		desired[importLinkExternalID(item.ExternalID, l)] = l
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, external_id, link_id FROM import_mappings WHERE project_id = ? AND source = ? AND object_type = 'link' AND issue_id = ?`, p.ProjectID, p.Source, issue.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("list source links: %w", err)
	}
	type sourceLinkMapping struct {
		id         int64
		externalID string
		linkID     sql.NullInt64
	}
	var sourceMappings []sourceLinkMapping
	for rows.Next() {
		var m sourceLinkMapping
		if err := rows.Scan(&m.id, &m.externalID, &m.linkID); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("scan source link mapping: %w", err)
		}
		sourceMappings = append(sourceMappings, m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}

	mappedLinks := map[string]int64{}
	for _, m := range sourceMappings {
		if importLink, keep := desired[m.externalID]; keep {
			if m.linkID.Valid {
				matches, err := importLinkMappingMatches(ctx, tx, p, issue, importLink, states, m.linkID.Int64)
				if err != nil {
					return nil, 0, err
				}
				if matches {
					mappedLinks[m.externalID] = m.linkID.Int64
					continue
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings WHERE id = ?`, m.id); err != nil {
				return nil, 0, fmt.Errorf("delete stale source link mapping: %w", err)
			}
			continue
		}
		if m.linkID.Valid {
			link, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE id = ?`, m.linkID.Int64))
			if err == nil {
				if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, link.ID); err != nil {
					return nil, 0, fmt.Errorf("delete source link: %w", err)
				}
				evt, err := d.insertLinkEvent(ctx, tx, p, issue, projectName, "issue.unlinked", link, item.UpdatedAt)
				if err != nil {
					return nil, 0, err
				}
				events = append(events, evt)
			} else if !errors.Is(err, ErrNotFound) {
				return nil, 0, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings WHERE id = ?`, m.id); err != nil {
			return nil, 0, fmt.Errorf("delete source link mapping: %w", err)
		}
	}

	for externalID, importLink := range desired {
		if _, ok := mappedLinks[externalID]; ok {
			continue
		}
		fromID, toID, err := importLinkEndpoints(ctx, tx, p, issue, importLink, states)
		if err != nil {
			return nil, 0, err
		}
		if _, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`, fromID, toID, importLink.Type)); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return nil, 0, err
		}
		createdAt := item.UpdatedAt
		if createdAt.IsZero() {
			createdAt = item.CreatedAt
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author, created_at)
			VALUES(?, ?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?, ?)`,
			p.ProjectID, fromID, toID, fromID, toID, importLink.Type, p.Actor, createdAt)
		if err != nil {
			return nil, 0, classifyLinkInsertError(err)
		}
		linkID, err := res.LastInsertId()
		if err != nil {
			return nil, 0, fmt.Errorf("last link id: %w", err)
		}
		_, err = upsertImportMapping(ctx, tx, ImportMappingParams{Source: p.Source, ExternalID: externalID, ObjectType: "link", ProjectID: p.ProjectID, IssueID: &issue.ID, LinkID: &linkID, SourceUpdatedAt: &item.UpdatedAt})
		if err != nil {
			return nil, 0, err
		}
		link, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE id = ?`, linkID))
		if err != nil {
			return nil, 0, err
		}
		evt, err := d.insertLinkEvent(ctx, tx, p, issue, projectName, "issue.linked", link, item.UpdatedAt)
		if err != nil {
			return nil, 0, err
		}
		events = append(events, evt)
		created++
	}
	return events, created, nil
}

func (d *DB) insertLinkEvent(ctx context.Context, tx *sql.Tx, p ImportBatchParams, issue Issue, projectName, eventType string, link Link, updatedAt time.Time) (Event, error) {
	relatedID := link.ToIssueID
	if relatedID == issue.ID {
		relatedID = link.FromIssueID
	}
	toShortID, toUID, err := issueIdentByID(ctx, tx, relatedID)
	if err != nil {
		return Event{}, err
	}
	// from_uid / to_uid match the live link-event shape from
	// queries_links.go — without them, TUI SSE refresh paths that key
	// parent-pane invalidation on payload UIDs miss import-generated
	// updates.
	payload, err := json.Marshal(map[string]any{
		"source":        p.Source,
		"link_id":       link.ID,
		"type":          link.Type,
		"from_short_id": issue.ShortID,
		"from_uid":      issue.UID,
		"to_short_id":   toShortID,
		"to_uid":        toUID,
		"updated_at":    updatedAt.UTC().Format(sqliteTimeFormat),
	})
	if err != nil {
		return Event{}, fmt.Errorf("marshal link payload: %w", err)
	}
	return d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectName: projectName, IssueID: &issue.ID, RelatedIssueID: &relatedID, Type: eventType, Actor: p.Actor, Payload: string(payload)})
}

// issueIdentByID returns the (short_id, uid) pair for an issue. Used by
// import path so link-event payloads carry the same identity pair the
// live daemon emits.
func issueIdentByID(ctx context.Context, tx *sql.Tx, issueID int64) (string, string, error) {
	var shortID, uid string
	if err := tx.QueryRowContext(ctx, `SELECT short_id, uid FROM issues WHERE id = ?`, issueID).Scan(&shortID, &uid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("lookup issue ident: %w", err)
	}
	return shortID, uid, nil
}

func importedIssueUpdatedPayload(source, externalID string, existing Issue, item ImportItem) (string, error) {
	payload := map[string]any{
		"source":      source,
		"external_id": externalID,
		"updated_at":  item.UpdatedAt.UTC().Format(sqliteTimeFormat),
	}
	if item.Title != existing.Title {
		payload["title"] = item.Title
		payload["old_title"] = existing.Title
	}
	if item.Body != existing.Body {
		payload["body"] = item.Body
	}
	newOwner := normalizeOwner(item.Owner)
	if !ownerEqual(existing.Owner, newOwner) {
		if newOwner == nil {
			payload["owner"] = nil
		} else {
			payload["owner"] = *newOwner
		}
		if existing.Owner == nil {
			payload["old_owner"] = nil
		} else {
			payload["old_owner"] = *existing.Owner
		}
	}
	if !priorityEqual(existing.Priority, item.Priority) {
		if item.Priority == nil {
			payload["priority"] = nil
		} else {
			payload["priority"] = *item.Priority
		}
		if existing.Priority == nil {
			payload["old_priority"] = nil
		} else {
			payload["old_priority"] = *existing.Priority
		}
	}
	if item.Status != existing.Status {
		payload["status"] = item.Status
	}
	if !stringPtrEqual(existing.ClosedReason, item.ClosedReason) {
		payload["closed_reason"] = stringPtrPayload(item.ClosedReason)
	}
	if !timePtrEqual(existing.ClosedAt, item.ClosedAt) {
		payload["closed_at"] = stringPtrPayload(formatOptionalSQLiteTime(item.ClosedAt))
	}
	bs, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal import event payload: %w", err)
	}
	return string(bs), nil
}

func stringPtrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.UTC().Format(sqliteTimeFormat) == b.UTC().Format(sqliteTimeFormat)
}

func stringPtrPayload(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func validImportClosedReason(reason string) bool {
	switch reason {
	case "done", "wontfix", "duplicate", "superseded", "audit-no-change":
		return true
	default:
		return false
	}
}

func validImportLabel(label string) bool {
	if len(label) < 1 || len(label) > 64 {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-':
		default:
			return false
		}
	}
	return true
}

func importLabelExternalID(issueExternalID, label string) string {
	return issueExternalID + ":label:" + label
}

func importLinkExternalID(issueExternalID string, link ImportLink) string {
	return issueExternalID + ":" + link.Type + ":" + link.TargetExternalID
}

func normalizeOwner(owner *string) *string {
	if owner == nil || *owner == "" {
		return nil
	}
	return owner
}

func importLinkMappingMatches(ctx context.Context, tx *sql.Tx, p ImportBatchParams, issue Issue, importLink ImportLink, states map[string]*importIssueState, linkID int64) (bool, error) {
	fromID, toID, err := importLinkEndpoints(ctx, tx, p, issue, importLink, states)
	if err != nil {
		return false, err
	}
	link, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE id = ?`, linkID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return link.FromIssueID == fromID && link.ToIssueID == toID && link.Type == importLink.Type, nil
}

func importLinkEndpoints(ctx context.Context, tx *sql.Tx, p ImportBatchParams, issue Issue, importLink ImportLink, states map[string]*importIssueState) (int64, int64, error) {
	targetIssue, err := resolveImportLinkTarget(ctx, tx, p, states, importLink.TargetExternalID)
	if err != nil {
		return 0, 0, err
	}
	fromID, toID := issue.ID, targetIssue.ID
	if importLink.Type == "related" && fromID > toID {
		fromID, toID = toID, fromID
	}
	return fromID, toID, nil
}

func resolveImportLinkTarget(ctx context.Context, tx *sql.Tx, p ImportBatchParams, states map[string]*importIssueState, externalID string) (Issue, error) {
	if state, ok := states[externalID]; ok {
		return state.issue, nil
	}
	mapping, err := importMappingBySource(ctx, tx, p.ProjectID, p.Source, "issue", externalID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Issue{}, fmt.Errorf("%w: import link target %q", ErrNotFound, externalID)
		}
		return Issue{}, err
	}
	if mapping.IssueID == nil {
		return Issue{}, fmt.Errorf("%w: import link target %q", ErrNotFound, externalID)
	}
	issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ? AND i.deleted_at IS NULL`, *mapping.IssueID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Issue{}, fmt.Errorf("%w: import link target %q", ErrNotFound, externalID)
		}
		return Issue{}, err
	}
	return issue, nil
}
