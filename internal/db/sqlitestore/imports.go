package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

type importIssueState struct {
	item                db.ImportItem
	issue               db.Issue
	created             bool
	sourceNewer         bool
	healed              bool
	presentationUpdated bool
}

// ImportBatch imports external issues atomically. Issues and comments are
// upserted through import_mappings; labels and links managed by this source are
// reconciled only when the source issue version is newer than kata's row (or the
// issue is newly created).
func (d *Store) ImportBatch(ctx context.Context, p db.ImportBatchParams) (db.ImportBatchResult, []db.Event, error) {
	return retryWrite2(ctx, d, func() (db.ImportBatchResult, []db.Event, error) {
		return d.importBatch(ctx, p)
	})
}

func (d *Store) importBatch(ctx context.Context, p db.ImportBatchParams) (db.ImportBatchResult, []db.Event, error) {
	if err := db.ValidateImportBatch(p); err != nil {
		return db.ImportBatchResult{}, nil, err
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.ImportBatchResult{}, nil, fmt.Errorf("begin import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var projectName, projectUID string
	if err := tx.QueryRowContext(ctx,
		`SELECT name, uid FROM projects WHERE id = ? AND deleted_at IS NULL`, p.ProjectID).
		Scan(&projectName, &projectUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.ImportBatchResult{}, nil, db.ErrNotFound
		}
		return db.ImportBatchResult{}, nil, fmt.Errorf("lookup import project: %w", err)
	}
	if err := ensureProjectWritableTx(ctx, tx, p.ProjectID); err != nil {
		return db.ImportBatchResult{}, nil, err
	}
	if err := validateIssueSyncImportGuardTx(ctx, tx, p); err != nil {
		return db.ImportBatchResult{}, nil, err
	}
	p, err = d.normalizeBoundFederationImportActorTx(ctx, tx, p)
	if err != nil {
		return db.ImportBatchResult{}, nil, err
	}

	result := db.ImportBatchResult{Source: p.Source, Items: make([]db.ImportItemResult, 0, len(p.Items)), Errors: []string{}}
	events := []db.Event{}
	states := make(map[string]*importIssueState, len(p.Items))

	for _, item := range p.Items {
		state, evt, err := d.importIssue(ctx, tx, p, item, projectName, projectUID)
		if err != nil {
			return db.ImportBatchResult{}, nil, err
		}
		if evt != nil {
			events = append(events, *evt)
		}
		states[item.ExternalID] = state
		switch {
		case state.created:
			result.Created++
		case state.sourceNewer, state.healed, state.presentationUpdated:
			result.Updated++
		default:
			result.Unchanged++
		}
		status := "unchanged"
		if state.created {
			status = "created"
		} else if state.sourceNewer || state.healed || state.presentationUpdated {
			status = "updated"
		}
		result.Items = append(result.Items, db.ImportItemResult{ExternalID: item.ExternalID, IssueShortID: state.issue.ShortID, Status: status})
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
			return db.ImportBatchResult{}, nil, err
		}
		events = append(events, commentEvents...)
		result.Comments += n
		if state.created || state.sourceNewer {
			labelEvents, err := d.reconcileImportLabels(ctx, tx, p, state.issue, item, projectName)
			if err != nil {
				return db.ImportBatchResult{}, nil, err
			}
			events = append(events, labelEvents...)
		}
	}

	for _, item := range p.Items {
		state := states[item.ExternalID]
		if state == nil {
			continue
		}
		linkTypeFilter, reconcileLinks := db.ImportLinkReconcileFilter(p, item, state.created, state.sourceNewer)
		if reconcileLinks {
			linkEvents, n, err := d.reconcileImportLinks(ctx, tx, p, state.issue, item, states, projectName, linkTypeFilter)
			if err != nil {
				return db.ImportBatchResult{}, nil, err
			}
			events = append(events, linkEvents...)
			result.Links += n
		}
	}

	if err := tx.Commit(); err != nil {
		return db.ImportBatchResult{}, nil, fmt.Errorf("commit import: %w", err)
	}
	return result, events, nil
}

func validateIssueSyncImportGuardTx(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams) error {
	if p.IssueSyncGuard == nil {
		return nil
	}
	g := p.IssueSyncGuard
	provider := strings.TrimSpace(g.Provider)
	if g.BindingID <= 0 || provider == "" || g.StartedAt.IsZero() {
		return fmt.Errorf("%w: invalid issue sync import guard", db.ErrImportValidation)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issue_sync_status SET binding_id = binding_id WHERE binding_id = ?`,
		g.BindingID); err != nil {
		return fmt.Errorf("lock issue sync import guard: %w", err)
	}
	if err := rejectFederationSpokeIssueSyncProject(ctx, tx, p.ProjectID); err != nil {
		return err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
		SELECT b.id
		  FROM issue_sync_bindings b
		  JOIN issue_sync_status s ON s.binding_id = b.id
		  JOIN projects p ON p.id = b.project_id
		 WHERE b.id = ?
		   AND b.project_id = ?
		   AND b.provider = ?
		   AND b.enabled = 1
		   AND p.deleted_at IS NULL
		   AND s.sync_started_at = ?`,
		g.BindingID, p.ProjectID, provider, g.StartedAt.UTC().Format(sqliteTimeFormat)).
		Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check issue sync import guard: %w", err)
	}
	binding, bindingErr := issueSyncBindingByID(ctx, tx, g.BindingID)
	if errors.Is(bindingErr, db.ErrNotFound) {
		return db.ErrIssueSyncNotEnabled
	}
	if bindingErr != nil {
		return bindingErr
	}
	if binding.ProjectID != p.ProjectID || binding.Provider != provider || !binding.Enabled {
		return db.ErrIssueSyncNotEnabled
	}
	return db.ErrIssueSyncAlreadyRunning
}

func (d *Store) normalizeBoundFederationImportActorTx(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams) (db.ImportBatchParams, error) {
	actor, ok, err := d.boundFederationActorTx(ctx, tx, p.ProjectID)
	if err != nil || !ok {
		return p, err
	}
	p.Actor = actor
	p.Items = append([]db.ImportItem(nil), p.Items...)
	for i := range p.Items {
		p.Items[i].Author = actor
		if len(p.Items[i].Comments) == 0 {
			continue
		}
		p.Items[i].Comments = append([]db.ImportComment(nil), p.Items[i].Comments...)
		for j := range p.Items[i].Comments {
			p.Items[i].Comments[j].Author = actor
		}
	}
	return p, nil
}

func (d *Store) importIssue(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, item db.ImportItem, projectName, projectUID string) (*importIssueState, *db.Event, error) {
	mapping, found, err := adoptImportMapping(ctx, tx, p.ProjectID, p.Source, "issue", item.ExternalID, item.LegacyExternalIDs)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		issue, evt, err := d.insertImportedIssue(ctx, tx, p, item, projectName, projectUID)
		if err != nil {
			return nil, nil, err
		}
		_, err = upsertImportMapping(ctx, tx, db.ImportMappingParams{Source: p.Source, ExternalID: item.ExternalID, ObjectType: "issue", ProjectID: p.ProjectID, IssueID: &issue.ID, SourceUpdatedAt: &item.UpdatedAt})
		if err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: issue, created: true, sourceNewer: true}, &evt, nil
	}
	if mapping.IssueID == nil {
		return nil, nil, fmt.Errorf("%w: issue mapping missing issue_id", db.ErrNotFound)
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
		_, err = upsertImportMapping(ctx, tx, db.ImportMappingParams{Source: p.Source, ExternalID: item.ExternalID, ObjectType: "issue", ProjectID: p.ProjectID, IssueID: &updated.ID, SourceUpdatedAt: &item.UpdatedAt})
		if err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: updated, sourceNewer: true}, &evt, nil
	}
	// The source is not newer overall, but a corrected earlier created_at must
	// still heal a row whose stored created_at was synthesized late by an older
	// sync (and may have left created_at after closed_at). Heal created_at only,
	// without overwriting other fields from this stale source item.
	if item.CreatedAt.Before(existing.CreatedAt) {
		healed, evt, err := d.healImportedCreatedAt(ctx, tx, p, item, existing, projectName)
		if err != nil {
			return nil, nil, err
		}
		_, err = upsertImportMapping(ctx, tx, db.ImportMappingParams{Source: p.Source, ExternalID: item.ExternalID, ObjectType: "issue", ProjectID: p.ProjectID, IssueID: &healed.ID, SourceUpdatedAt: &item.UpdatedAt})
		if err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: healed, healed: true}, &evt, nil
	}
	if db.ImportOwnsSameSourceVersionTitle(mapping, existing, item) {
		updated, evt, err := d.updateImportedPresentationTitle(ctx, tx, p, item, existing, projectName)
		if err != nil {
			return nil, nil, err
		}
		_, err = upsertImportMapping(ctx, tx, db.ImportMappingParams{Source: p.Source, ExternalID: item.ExternalID, ObjectType: "issue", ProjectID: p.ProjectID, IssueID: &updated.ID, SourceUpdatedAt: &item.UpdatedAt})
		if err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: updated, presentationUpdated: true}, &evt, nil
	}
	_, err = upsertImportMapping(ctx, tx, db.ImportMappingParams{Source: p.Source, ExternalID: item.ExternalID, ObjectType: "issue", ProjectID: p.ProjectID, IssueID: &existing.ID, SourceUpdatedAt: &item.UpdatedAt})
	if err != nil {
		return nil, nil, err
	}
	return &importIssueState{item: item, issue: existing}, nil, nil
}

func (d *Store) insertImportedIssue(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, item db.ImportItem, projectName, projectUID string) (db.Issue, db.Event, error) {
	// Validate project exists and is not archived.
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE id = ? AND deleted_at IS NULL`, p.ProjectID).
		Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.Issue{}, db.Event{}, db.ErrNotFound
		}
		return db.Issue{}, db.Event{}, fmt.Errorf("check project: %w", err)
	}
	issueUID, err := katauid.New()
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("generate issue uid: %w", err)
	}
	shortID, err := assignShortID(ctx, tx, p.ProjectID, issueUID)
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("assign import short_id: %w", err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO issues(uid, project_id, short_id, title, body, status, closed_reason, owner, author, created_at, updated_at, closed_at, priority)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		issueUID, p.ProjectID, shortID, item.Title, item.Body, item.Status, item.ClosedReason, db.NormalizeImportOwner(item.Owner), item.Author, item.CreatedAt, item.UpdatedAt, item.ClosedAt, item.Priority)
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("insert imported issue: %w", err)
	}
	issueID, err := res.LastInsertId()
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("last issue id: %w", err)
	}
	payload, err := buildIssueCreatedPayload(issueCreatedPayload{
		UID:          issueUID,
		ShortID:      shortID,
		Title:        item.Title,
		Body:         item.Body,
		Author:       item.Author,
		Owner:        db.NormalizeImportOwner(item.Owner),
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
		return db.Issue{}, db.Event{}, err
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectUID: projectUID, ProjectName: projectName, IssueID: &issueID, IssueUID: &issueUID, Type: "issue.created", Actor: p.Actor, Payload: payload})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, issueID))
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	return issue, evt, nil
}

func (d *Store) updateImportedIssue(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, item db.ImportItem, existing db.Issue, projectName string) (db.Issue, db.Event, error) {
	// created_at only ever moves earlier. A corrected source timestamp heals
	// rows whose stored created_at was synthesized late by an older sync (and
	// could otherwise outrun a freshly written closed_at); a later synthetic
	// value never pushes the genuine creation time forward.
	createdAt := existing.CreatedAt
	if item.CreatedAt.Before(createdAt) {
		createdAt = item.CreatedAt
	}
	// Bump content_revision only when the embeddable text actually changes,
	// mirroring editIssue. The fragment is a fixed literal with no bound
	// value, so it never widens the args list. See docs/design/semantic-search.md.
	bump := ""
	if item.Title != existing.Title || item.Body != existing.Body {
		bump = `, content_revision = content_revision + 1`
	}
	_, err := tx.ExecContext(ctx, `UPDATE issues
		SET title = ?, body = ?, status = ?, closed_reason = ?, owner = ?, created_at = ?, updated_at = ?, closed_at = ?, priority = ?`+bump+`
		WHERE id = ?`, item.Title, item.Body, item.Status, item.ClosedReason, db.NormalizeImportOwner(item.Owner), createdAt, item.UpdatedAt, item.ClosedAt, item.Priority, existing.ID)
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("update imported issue: %w", err)
	}
	payload, err := db.ImportedIssueUpdatedPayload(p.Source, item.ExternalID, existing, item)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectName: projectName, IssueID: &existing.ID, Type: "issue.updated", Actor: p.Actor, Payload: payload})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	updated, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, existing.ID))
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	return updated, evt, nil
}

func (d *Store) updateImportedPresentationTitle(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, item db.ImportItem, existing db.Issue, projectName string) (db.Issue, db.Event, error) {
	_, err := tx.ExecContext(ctx, `UPDATE issues SET title = ?, content_revision = content_revision + 1 WHERE id = ?`, item.Title, existing.ID)
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("update imported title: %w", err)
	}
	payload, err := db.ImportedPresentationTitlePayload(p.Source, item.ExternalID, existing, item)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectName: projectName, IssueID: &existing.ID, Type: "issue.updated", Actor: p.Actor, Payload: payload})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	updated, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, existing.ID))
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	return updated, evt, nil
}

// healImportedCreatedAt repairs a row whose stored created_at is later than the
// corrected source timestamp, without touching any other field. It emits an
// issue.updated event carrying only created_at so event-folded and federated
// projections heal the same way the local row does.
func (d *Store) healImportedCreatedAt(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, item db.ImportItem, existing db.Issue, projectName string) (db.Issue, db.Event, error) {
	_, err := tx.ExecContext(ctx, `UPDATE issues SET created_at = ? WHERE id = ?`, item.CreatedAt, existing.ID)
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("heal imported created_at: %w", err)
	}
	payload, err := db.ImportedCreatedAtHealedPayload(p.Source, item.ExternalID, existing, item)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	evt, err := d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectName: projectName, IssueID: &existing.ID, Type: "issue.updated", Actor: p.Actor, Payload: payload})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	healed, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, existing.ID))
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	return healed, evt, nil
}

func (d *Store) importComments(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, issue db.Issue, item db.ImportItem, projectName string) ([]db.Event, int, error) {
	events := []db.Event{}
	created := 0
	for _, c := range item.Comments {
		mapping, found, err := adoptImportMapping(ctx, tx, p.ProjectID, p.Source, "comment", c.ExternalID, c.LegacyExternalIDs)
		if err != nil {
			return nil, 0, err
		}
		if found {
			if mapping.IssueID != nil && *mapping.IssueID != issue.ID {
				return nil, 0, fmt.Errorf("%w: comment %q is mapped to a different issue", db.ErrImportValidation, c.ExternalID)
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
		_, err = upsertImportMapping(ctx, tx, db.ImportMappingParams{Source: p.Source, ExternalID: c.ExternalID, ObjectType: "comment", ProjectID: p.ProjectID, IssueID: &issue.ID, CommentID: &commentID})
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

func (d *Store) reconcileImportLabels(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, issue db.Issue, item db.ImportItem, projectName string) ([]db.Event, error) {
	events := []db.Event{}
	desired := map[string]string{}
	for _, label := range dedupeStrings(item.Labels) {
		desired[label] = db.ImportLabelExternalID(item.ExternalID, label)
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
			_, err = upsertImportMapping(ctx, tx, db.ImportMappingParams{Source: p.Source, ExternalID: externalID, ObjectType: "label", ProjectID: p.ProjectID, IssueID: &issue.ID, Label: &label, SourceUpdatedAt: &item.UpdatedAt})
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

func (d *Store) insertLabelEvent(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, issue db.Issue, projectName, itemExternalID, eventType, label string, updatedAt time.Time) (db.Event, error) {
	payload, err := json.Marshal(map[string]any{
		"issue_uid":   issue.UID,
		"source":      p.Source,
		"external_id": db.ImportLabelExternalID(itemExternalID, label),
		"label":       label,
		"updated_at":  updatedAt.UTC().Format(sqliteTimeFormat),
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal label payload: %w", err)
	}
	return d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ProjectID, ProjectName: projectName, IssueID: &issue.ID, Type: eventType, Actor: p.Actor, Payload: string(payload)})
}

func (d *Store) reconcileImportLinks(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, issue db.Issue, item db.ImportItem, states map[string]*importIssueState, projectName string, linkTypeFilter map[string]bool) ([]db.Event, int, error) {
	events := []db.Event{}
	created := 0
	desired := map[string]db.ImportLink{}
	for _, l := range item.Links {
		if !db.ImportLinkTypeAllowed(linkTypeFilter, l.Type) {
			continue
		}
		desired[db.ImportLinkExternalID(item.ExternalID, l)] = l
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
				if !db.ImportLinkTypeAllowed(linkTypeFilter, link.Type) {
					continue
				}
				if !db.ImportItemLinkTypeAuthoritative(item, link.Type) {
					continue
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, link.ID); err != nil {
					return nil, 0, fmt.Errorf("delete source link: %w", err)
				}
				evt, err := d.insertLinkEvent(ctx, tx, p, issue, projectName, "issue.unlinked", link, item.UpdatedAt)
				if err != nil {
					return nil, 0, err
				}
				events = append(events, evt)
			} else if !errors.Is(err, db.ErrNotFound) {
				return nil, 0, err
			}
		} else if !db.ImportLinkMappingExternalIDAllowed(item.ExternalID, m.externalID, linkTypeFilter) {
			continue
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
		} else if !errors.Is(err, db.ErrNotFound) {
			return nil, 0, err
		}
		createdAt := item.UpdatedAt
		if createdAt.IsZero() {
			createdAt = item.CreatedAt
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO links(from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author, created_at)
			VALUES(?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?, ?)`,
			fromID, toID, fromID, toID, importLink.Type, p.Actor, createdAt)
		if err != nil {
			classified := classifyLinkInsertError(err)
			if errors.Is(classified, db.ErrParentAlreadySet) {
				if p.PreserveLocalParentConflicts {
					continue
				}
			}
			return nil, 0, classified
		}
		linkID, err := res.LastInsertId()
		if err != nil {
			return nil, 0, fmt.Errorf("last link id: %w", err)
		}
		_, err = upsertImportMapping(ctx, tx, db.ImportMappingParams{Source: p.Source, ExternalID: externalID, ObjectType: "link", ProjectID: p.ProjectID, IssueID: &issue.ID, LinkID: &linkID, SourceUpdatedAt: &item.UpdatedAt})
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

func (d *Store) insertLinkEvent(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, issue db.Issue, projectName, eventType string, link db.Link, updatedAt time.Time) (db.Event, error) {
	relatedID := link.ToIssueID
	if relatedID == issue.ID {
		relatedID = link.FromIssueID
	}
	toShortID, toUID, err := issueIdentByID(ctx, tx, relatedID)
	if err != nil {
		return db.Event{}, err
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
		return db.Event{}, fmt.Errorf("marshal link payload: %w", err)
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
			return "", "", db.ErrNotFound
		}
		return "", "", fmt.Errorf("lookup issue ident: %w", err)
	}
	return shortID, uid, nil
}

func importLinkMappingMatches(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, issue db.Issue, importLink db.ImportLink, states map[string]*importIssueState, linkID int64) (bool, error) {
	fromID, toID, err := importLinkEndpoints(ctx, tx, p, issue, importLink, states)
	if err != nil {
		return false, err
	}
	link, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE id = ?`, linkID))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return link.FromIssueID == fromID && link.ToIssueID == toID && link.Type == importLink.Type, nil
}

func importLinkEndpoints(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, issue db.Issue, importLink db.ImportLink, states map[string]*importIssueState) (int64, int64, error) {
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

func resolveImportLinkTarget(ctx context.Context, tx *sql.Tx, p db.ImportBatchParams, states map[string]*importIssueState, externalID string) (db.Issue, error) {
	if state, ok := states[externalID]; ok {
		return state.issue, nil
	}
	mapping, err := importMappingBySource(ctx, tx, p.ProjectID, p.Source, "issue", externalID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Issue{}, fmt.Errorf("%w: import link target %q", db.ErrNotFound, externalID)
		}
		return db.Issue{}, err
	}
	if mapping.IssueID == nil {
		return db.Issue{}, fmt.Errorf("%w: import link target %q", db.ErrNotFound, externalID)
	}
	issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ? AND i.deleted_at IS NULL`, *mapping.IssueID))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Issue{}, fmt.Errorf("%w: import link target %q", db.ErrNotFound, externalID)
		}
		return db.Issue{}, err
	}
	return issue, nil
}
