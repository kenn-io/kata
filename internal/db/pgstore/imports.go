package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// ImportBatch atomically reconciles one normalized external issue batch.
func (s *Store) ImportBatch(
	ctx context.Context,
	params db.ImportBatchParams,
) (db.ImportBatchResult, []db.Event, error) {
	if err := db.ValidateImportBatch(params); err != nil {
		return db.ImportBatchResult{}, nil, err
	}
	var result db.ImportBatchResult
	var events []db.Event
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		attemptParams := params
		result = db.ImportBatchResult{
			Source: attemptParams.Source,
			Items:  make([]db.ImportItemResult, 0, len(attemptParams.Items)),
			Errors: []string{},
		}
		events = []db.Event{}
		project, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, attemptParams.ProjectID))
		if err != nil {
			return err
		}
		if err := ensureProjectWritableTx(ctx, tx, attemptParams.ProjectID); err != nil {
			return err
		}
		if err := validateIssueSyncImportGuardTx(ctx, tx, attemptParams); err != nil {
			return err
		}
		attemptParams, err = normalizeBoundFederationImportActorTx(ctx, tx, attemptParams)
		if err != nil {
			return err
		}

		states := make(map[string]*importIssueState, len(attemptParams.Items))
		for _, item := range attemptParams.Items {
			state, event, err := s.importIssue(ctx, tx, attemptParams, item, project)
			if err != nil {
				return err
			}
			if event != nil {
				events = append(events, *event)
			}
			states[item.ExternalID] = state
			status := "unchanged"
			switch {
			case state.created:
				result.Created++
				status = "created"
			case state.sourceNewer, state.healed, state.presentationUpdated:
				result.Updated++
				status = "updated"
			default:
				result.Unchanged++
			}
			result.Items = append(result.Items, db.ImportItemResult{
				ExternalID: item.ExternalID, IssueShortID: state.issue.ShortID, Status: status,
			})
		}

		for _, item := range attemptParams.Items {
			state := states[item.ExternalID]
			if state == nil {
				continue
			}
			commentEvents, count, err := s.importComments(ctx, tx, attemptParams, state.issue, item, project)
			if err != nil {
				return err
			}
			events = append(events, commentEvents...)
			result.Comments += count
			if state.created || state.sourceNewer {
				labelEvents, err := s.reconcileImportLabels(ctx, tx, attemptParams, state.issue, item, project)
				if err != nil {
					return err
				}
				events = append(events, labelEvents...)
			}
		}

		for _, item := range attemptParams.Items {
			state := states[item.ExternalID]
			if state == nil {
				continue
			}
			filter, reconcile := db.ImportLinkReconcileFilter(
				attemptParams, item, state.created, state.sourceNewer,
			)
			if !reconcile {
				continue
			}
			linkEvents, count, err := s.reconcileImportLinks(
				ctx, tx, attemptParams, state.issue, item, states, project, filter,
			)
			if err != nil {
				return err
			}
			events = append(events, linkEvents...)
			result.Links += count
		}
		return nil
	})
	if err != nil {
		return db.ImportBatchResult{}, nil, err
	}
	return result, events, nil
}

func validateIssueSyncImportGuardTx(ctx context.Context, tx *sql.Tx, params db.ImportBatchParams) error {
	if params.IssueSyncGuard == nil {
		return nil
	}
	guard := params.IssueSyncGuard
	provider := strings.TrimSpace(guard.Provider)
	if guard.BindingID <= 0 || provider == "" || guard.StartedAt.IsZero() {
		return fmt.Errorf("%w: invalid issue sync import guard", db.ErrImportValidation)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issue_sync_status SET binding_id=binding_id WHERE binding_id=$1`, guard.BindingID); err != nil {
		return fmt.Errorf("lock issue sync import guard: %w", mapSQLError(err, nil))
	}
	if err := rejectFederationSpokeIssueSyncProject(ctx, tx, params.ProjectID); err != nil {
		return err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT b.id
FROM issue_sync_bindings b
JOIN issue_sync_status s ON s.binding_id=b.id
JOIN projects p ON p.id=b.project_id
WHERE b.id=$1 AND b.project_id=$2 AND b.provider=$3 AND b.enabled=1
  AND p.deleted_at IS NULL AND s.sync_started_at=$4`,
		guard.BindingID, params.ProjectID, provider, formatStoredTime(guard.StartedAt)).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check issue sync import guard: %w", mapSQLError(err, nil))
	}
	binding, bindingErr := scanIssueSyncBinding(tx.QueryRowContext(ctx,
		issueSyncBindingSelect+` WHERE b.id=$1`, guard.BindingID))
	if errors.Is(bindingErr, db.ErrNotFound) {
		return db.ErrIssueSyncNotEnabled
	}
	if bindingErr != nil {
		return bindingErr
	}
	if binding.ProjectID != params.ProjectID || binding.Provider != provider || !binding.Enabled {
		return db.ErrIssueSyncNotEnabled
	}
	return db.ErrIssueSyncAlreadyRunning
}

func normalizeBoundFederationImportActorTx(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
) (db.ImportBatchParams, error) {
	var actor string
	err := tx.QueryRowContext(ctx, `SELECT bound_actor FROM federation_bindings
WHERE project_id=$1 AND role=$2 AND enabled=1 AND push_enabled=1`,
		params.ProjectID, string(db.FederationRoleSpoke)).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		return params, nil
	}
	if err != nil {
		return params, fmt.Errorf("lookup bound federation actor: %w", mapSQLError(err, nil))
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return params, nil
	}
	params.Actor = actor
	params.Items = append([]db.ImportItem(nil), params.Items...)
	for index := range params.Items {
		params.Items[index].Author = actor
		if len(params.Items[index].Comments) == 0 {
			continue
		}
		params.Items[index].Comments = append([]db.ImportComment(nil), params.Items[index].Comments...)
		for commentIndex := range params.Items[index].Comments {
			params.Items[index].Comments[commentIndex].Author = actor
		}
	}
	return params, nil
}

func (s *Store) importIssue(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	item db.ImportItem,
	project db.Project,
) (*importIssueState, *db.Event, error) {
	mapping, found, err := adoptImportMappingTx(
		ctx, tx, params.ProjectID, params.Source, "issue", item.ExternalID, item.LegacyExternalIDs,
	)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		issue, event, err := s.insertImportedIssue(ctx, tx, params, item, project)
		if err != nil {
			return nil, nil, err
		}
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: params.Source, ExternalID: item.ExternalID, ObjectType: "issue",
			ProjectID: params.ProjectID, IssueID: &issue.ID, SourceUpdatedAt: &item.UpdatedAt,
		}); err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: issue, created: true, sourceNewer: true}, &event, nil
	}
	if mapping.IssueID == nil {
		return nil, nil, fmt.Errorf("%w: issue mapping missing issue_id", db.ErrNotFound)
	}
	existing, err := scanIssue(tx.QueryRowContext(ctx,
		issueSelect+` WHERE i.id=$1 AND i.deleted_at IS NULL FOR UPDATE OF i`, *mapping.IssueID))
	if err != nil {
		return nil, nil, err
	}
	if item.UpdatedAt.After(existing.UpdatedAt) {
		updated, event, err := s.updateImportedIssue(ctx, tx, params, item, existing, project)
		if err != nil {
			return nil, nil, err
		}
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: params.Source, ExternalID: item.ExternalID, ObjectType: "issue",
			ProjectID: params.ProjectID, IssueID: &updated.ID, SourceUpdatedAt: &item.UpdatedAt,
		}); err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: updated, sourceNewer: true}, &event, nil
	}
	if item.CreatedAt.Before(existing.CreatedAt) {
		healed, event, err := s.healImportedCreatedAt(ctx, tx, params, item, existing, project)
		if err != nil {
			return nil, nil, err
		}
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: params.Source, ExternalID: item.ExternalID, ObjectType: "issue",
			ProjectID: params.ProjectID, IssueID: &healed.ID, SourceUpdatedAt: &item.UpdatedAt,
		}); err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: healed, healed: true}, &event, nil
	}
	if db.ImportOwnsSameSourceVersionTitle(mapping, existing, item) {
		updated, event, err := s.updateImportedPresentationTitle(ctx, tx, params, item, existing, project)
		if err != nil {
			return nil, nil, err
		}
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: params.Source, ExternalID: item.ExternalID, ObjectType: "issue",
			ProjectID: params.ProjectID, IssueID: &updated.ID, SourceUpdatedAt: &item.UpdatedAt,
		}); err != nil {
			return nil, nil, err
		}
		return &importIssueState{item: item, issue: updated, presentationUpdated: true}, &event, nil
	}
	if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
		Source: params.Source, ExternalID: item.ExternalID, ObjectType: "issue",
		ProjectID: params.ProjectID, IssueID: &existing.ID, SourceUpdatedAt: &item.UpdatedAt,
	}); err != nil {
		return nil, nil, err
	}
	return &importIssueState{item: item, issue: existing}, nil, nil
}

func (s *Store) insertImportedIssue(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	item db.ImportItem,
	project db.Project,
) (db.Issue, db.Event, error) {
	issueUID, err := katauid.New()
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("generate issue uid: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, params.ProjectID); err != nil {
		return db.Issue{}, db.Event{}, mapSQLError(err, nil)
	}
	shortID, err := s.resolveShortIDTx(ctx, tx, params.ProjectID, issueUID, "")
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("assign import short_id: %w", err)
	}
	createdAt := formatStoredTime(item.CreatedAt)
	updatedAt := formatStoredTime(item.UpdatedAt)
	var closedAt any
	if item.ClosedAt != nil {
		closedAt = formatStoredTime(*item.ClosedAt)
	}
	var issueID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO issues(
uid,project_id,short_id,title,body,status,closed_reason,owner,author,
created_at,updated_at,closed_at,priority
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		issueUID, params.ProjectID, shortID, item.Title, item.Body, item.Status, item.ClosedReason,
		db.NormalizeImportOwner(item.Owner), item.Author, createdAt, updatedAt, closedAt, item.Priority).Scan(&issueID)
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("insert imported issue: %w", mapSQLError(err, nil))
	}
	payload, err := json.Marshal(issueCreatedPayload{
		UID: issueUID, ShortID: shortID, Title: item.Title, Body: item.Body,
		Author: item.Author, Owner: db.NormalizeImportOwner(item.Owner), Priority: item.Priority,
		Status: item.Status, ClosedReason: item.ClosedReason, ClosedAt: optionalStoredTime(item.ClosedAt),
		Metadata: json.RawMessage(`{}`), CreatedAt: createdAt, UpdatedAt: updatedAt,
		Source: params.Source, ExternalID: item.ExternalID,
	})
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("marshal issue.created payload: %w", err)
	}
	event, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		IssueID: &issueID, IssueUID: &issueUID, Type: "issue.created", Actor: params.Actor,
		Payload: string(payload),
	})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1`, issueID))
	return issue, event, err
}

func (s *Store) updateImportedIssue(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	item db.ImportItem,
	existing db.Issue,
	project db.Project,
) (db.Issue, db.Event, error) {
	createdAt := existing.CreatedAt
	if item.CreatedAt.Before(createdAt) {
		createdAt = item.CreatedAt
	}
	bump := ""
	if item.Title != existing.Title || item.Body != existing.Body {
		bump = `,content_revision=content_revision+1`
	}
	var closedAt any
	if item.ClosedAt != nil {
		closedAt = formatStoredTime(*item.ClosedAt)
	}
	_, err := tx.ExecContext(ctx, `UPDATE issues SET
title=$1,body=$2,status=$3,closed_reason=$4,owner=$5,created_at=$6,
updated_at=$7,closed_at=$8,priority=$9`+bump+` WHERE id=$10`,
		item.Title, item.Body, item.Status, item.ClosedReason, db.NormalizeImportOwner(item.Owner),
		formatStoredTime(createdAt), formatStoredTime(item.UpdatedAt), closedAt, item.Priority, existing.ID)
	if err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("update imported issue: %w", mapSQLError(err, nil))
	}
	payload, err := db.ImportedIssueUpdatedPayload(params.Source, item.ExternalID, existing, item)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	event, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		IssueID: &existing.ID, IssueUID: &existing.UID, Type: "issue.updated",
		Actor: params.Actor, Payload: payload,
	})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	updated, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1`, existing.ID))
	return updated, event, err
}

func (s *Store) updateImportedPresentationTitle(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	item db.ImportItem,
	existing db.Issue,
	project db.Project,
) (db.Issue, db.Event, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET title=$1,content_revision=content_revision+1 WHERE id=$2`, item.Title, existing.ID); err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("update imported title: %w", mapSQLError(err, nil))
	}
	payload, err := db.ImportedPresentationTitlePayload(params.Source, item.ExternalID, existing, item)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	event, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		IssueID: &existing.ID, IssueUID: &existing.UID, Type: "issue.updated",
		Actor: params.Actor, Payload: payload,
	})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	updated, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1`, existing.ID))
	return updated, event, err
}

func (s *Store) healImportedCreatedAt(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	item db.ImportItem,
	existing db.Issue,
	project db.Project,
) (db.Issue, db.Event, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE issues SET created_at=$1 WHERE id=$2`, formatStoredTime(item.CreatedAt), existing.ID); err != nil {
		return db.Issue{}, db.Event{}, fmt.Errorf("heal imported created_at: %w", mapSQLError(err, nil))
	}
	payload, err := db.ImportedCreatedAtHealedPayload(params.Source, item.ExternalID, existing, item)
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	event, err := s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		IssueID: &existing.ID, IssueUID: &existing.UID, Type: "issue.updated",
		Actor: params.Actor, Payload: payload,
	})
	if err != nil {
		return db.Issue{}, db.Event{}, err
	}
	healed, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1`, existing.ID))
	return healed, event, err
}
