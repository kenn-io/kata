package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

func (s *Store) importComments(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	issue db.Issue,
	item db.ImportItem,
	project db.Project,
) ([]db.Event, int, error) {
	events := []db.Event{}
	created := 0
	for _, commentInput := range item.Comments {
		mapping, found, err := adoptImportMappingTx(
			ctx, tx, params.ProjectID, params.Source, "comment",
			commentInput.ExternalID, commentInput.LegacyExternalIDs,
		)
		if err != nil {
			return nil, 0, err
		}
		if found {
			if mapping.IssueID != nil && *mapping.IssueID != issue.ID {
				return nil, 0, fmt.Errorf(
					"%w: comment %q is mapped to a different issue",
					db.ErrImportValidation, commentInput.ExternalID,
				)
			}
			continue
		}
		commentUID, err := katauid.New()
		if err != nil {
			return nil, 0, fmt.Errorf("generate imported comment uid: %w", err)
		}
		var commentID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO comments(uid,issue_id,author,body,created_at)
VALUES($1,$2,$3,$4,$5) RETURNING id`, commentUID, issue.ID, commentInput.Author,
			commentInput.Body, formatStoredTime(commentInput.CreatedAt)).Scan(&commentID)
		if err != nil {
			return nil, 0, fmt.Errorf("insert imported comment: %w", mapSQLError(err, nil))
		}
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: params.Source, ExternalID: commentInput.ExternalID, ObjectType: "comment",
			ProjectID: params.ProjectID, IssueID: &issue.ID, CommentID: &commentID,
		}); err != nil {
			return nil, 0, err
		}
		payload, err := json.Marshal(map[string]any{
			"comment_uid": commentUID, "author": commentInput.Author, "body": commentInput.Body,
			"created_at": formatStoredTime(commentInput.CreatedAt), "source": params.Source,
			"external_id": item.ExternalID, "comment_external_id": commentInput.ExternalID,
		})
		if err != nil {
			return nil, 0, fmt.Errorf("marshal import comment payload: %w", err)
		}
		event, err := s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			IssueID: &issue.ID, IssueUID: &issue.UID, Type: "issue.commented",
			Actor: params.Actor, Payload: string(payload),
		})
		if err != nil {
			return nil, 0, err
		}
		events = append(events, event)
		created++
	}
	return events, created, nil
}

func (s *Store) reconcileImportLabels(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	issue db.Issue,
	item db.ImportItem,
	project db.Project,
) ([]db.Event, error) {
	events := []db.Event{}
	desired := map[string]string{}
	for _, label := range dedupeAndSort(item.Labels) {
		desired[label] = db.ImportLabelExternalID(item.ExternalID, label)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,external_id,label FROM import_mappings
WHERE project_id=$1 AND source=$2 AND object_type='label' AND issue_id=$3
ORDER BY id FOR UPDATE`, params.ProjectID, params.Source, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("list source labels: %w", mapSQLError(err, nil))
	}
	type labelMapping struct {
		id         int64
		externalID string
		label      sql.NullString
	}
	var mappings []labelMapping
	for rows.Next() {
		var mapping labelMapping
		if err := rows.Scan(&mapping.id, &mapping.externalID, &mapping.label); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan source label mapping: %w", mapSQLError(err, nil))
		}
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, mapSQLError(err, nil)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	existingMappings := map[string]int64{}
	for _, mapping := range mappings {
		if mapping.label.Valid {
			existingMappings[mapping.label.String] = mapping.id
		}
		if mapping.label.Valid && desired[mapping.label.String] == mapping.externalID {
			continue
		}
		if mapping.label.Valid {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM issue_labels WHERE issue_id=$1 AND label=$2`, issue.ID, mapping.label.String); err != nil {
				return nil, fmt.Errorf("delete source label: %w", mapSQLError(err, nil))
			}
			event, err := s.insertImportLabelEvent(
				ctx, tx, params, issue, project, item.ExternalID,
				"issue.unlabeled", mapping.label.String, item.UpdatedAt,
			)
			if err != nil {
				return nil, err
			}
			events = append(events, event)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings WHERE id=$1`, mapping.id); err != nil {
			return nil, fmt.Errorf("delete source label mapping: %w", mapSQLError(err, nil))
		}
	}

	for label, externalID := range desired {
		result, err := tx.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author,created_at)
VALUES($1,$2,$3,$4) ON CONFLICT(issue_id,label) DO NOTHING`,
			issue.ID, label, params.Actor, formatStoredTime(item.CreatedAt))
		if err != nil {
			return nil, mapSQLError(err, labelConstraintErrors)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("label rows affected: %w", err)
		}
		if _, exists := existingMappings[label]; exists || affected > 0 {
			if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
				Source: params.Source, ExternalID: externalID, ObjectType: "label",
				ProjectID: params.ProjectID, IssueID: &issue.ID, Label: &label,
				SourceUpdatedAt: &item.UpdatedAt,
			}); err != nil {
				return nil, err
			}
		}
		if affected > 0 {
			event, err := s.insertImportLabelEvent(
				ctx, tx, params, issue, project, item.ExternalID,
				"issue.labeled", label, item.UpdatedAt,
			)
			if err != nil {
				return nil, err
			}
			events = append(events, event)
		}
	}
	return events, nil
}

func (s *Store) insertImportLabelEvent(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	issue db.Issue,
	project db.Project,
	itemExternalID string,
	eventType string,
	label string,
	updatedAt time.Time,
) (db.Event, error) {
	payload, err := json.Marshal(map[string]any{
		"issue_uid": issue.UID, "source": params.Source,
		"external_id": db.ImportLabelExternalID(itemExternalID, label),
		"label":       label, "updated_at": formatStoredTime(updatedAt),
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal label payload: %w", err)
	}
	return s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		IssueID: &issue.ID, IssueUID: &issue.UID, Type: eventType,
		Actor: params.Actor, Payload: string(payload),
	})
}

func (s *Store) reconcileImportLinks(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	issue db.Issue,
	item db.ImportItem,
	states map[string]*importIssueState,
	project db.Project,
	linkTypeFilter map[string]bool,
) ([]db.Event, int, error) {
	events := []db.Event{}
	created := 0
	desired := map[string]db.ImportLink{}
	for _, link := range item.Links {
		if db.ImportLinkTypeAllowed(linkTypeFilter, link.Type) {
			desired[db.ImportLinkExternalID(item.ExternalID, link)] = link
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,external_id,link_id FROM import_mappings
WHERE project_id=$1 AND source=$2 AND object_type='link' AND issue_id=$3
ORDER BY id FOR UPDATE`, params.ProjectID, params.Source, issue.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("list source links: %w", mapSQLError(err, nil))
	}
	type sourceLinkMapping struct {
		id         int64
		externalID string
		linkID     sql.NullInt64
	}
	var sourceMappings []sourceLinkMapping
	for rows.Next() {
		var mapping sourceLinkMapping
		if err := rows.Scan(&mapping.id, &mapping.externalID, &mapping.linkID); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("scan source link mapping: %w", mapSQLError(err, nil))
		}
		sourceMappings = append(sourceMappings, mapping)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, mapSQLError(err, nil)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}

	mappedLinks := map[string]int64{}
	for _, mapping := range sourceMappings {
		if importLink, keep := desired[mapping.externalID]; keep {
			if mapping.linkID.Valid {
				matches, err := importLinkMappingMatches(
					ctx, tx, params, issue, importLink, states, mapping.linkID.Int64,
				)
				if err != nil {
					return nil, 0, err
				}
				if matches {
					mappedLinks[mapping.externalID] = mapping.linkID.Int64
					continue
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings WHERE id=$1`, mapping.id); err != nil {
				return nil, 0, fmt.Errorf("delete stale source link mapping: %w", mapSQLError(err, nil))
			}
			continue
		}
		if mapping.linkID.Valid {
			link, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE id=$1`, mapping.linkID.Int64))
			if err == nil {
				if !db.ImportLinkTypeAllowed(linkTypeFilter, link.Type) ||
					!db.ImportItemLinkTypeAuthoritative(item, link.Type) {
					continue
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id=$1`, link.ID); err != nil {
					return nil, 0, fmt.Errorf("delete source link: %w", mapSQLError(err, nil))
				}
				event, err := s.insertImportLinkEvent(
					ctx, tx, params, issue, project, "issue.unlinked", link, item.UpdatedAt,
				)
				if err != nil {
					return nil, 0, err
				}
				events = append(events, event)
			} else if !errors.Is(err, db.ErrNotFound) {
				return nil, 0, err
			}
		} else if !db.ImportLinkMappingExternalIDAllowed(item.ExternalID, mapping.externalID, linkTypeFilter) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM import_mappings WHERE id=$1`, mapping.id); err != nil {
			return nil, 0, fmt.Errorf("delete source link mapping: %w", mapSQLError(err, nil))
		}
	}

	for externalID, importLink := range desired {
		if _, exists := mappedLinks[externalID]; exists {
			continue
		}
		fromID, toID, err := importLinkEndpoints(ctx, tx, params, issue, importLink, states)
		if err != nil {
			return nil, 0, err
		}
		if _, err := scanLink(tx.QueryRowContext(ctx,
			linkSelect+` WHERE from_issue_id=$1 AND to_issue_id=$2 AND type=$3`,
			fromID, toID, importLink.Type)); err == nil {
			continue
		} else if !errors.Is(err, db.ErrNotFound) {
			return nil, 0, err
		}
		linkParams := db.CreateLinkParams{
			FromIssueID: fromID, ToIssueID: toID, Type: importLink.Type, Author: params.Actor,
		}
		if err := prepareLinkInsertTx(ctx, tx, linkParams, true); err != nil {
			if errors.Is(err, db.ErrParentAlreadySet) && params.PreserveLocalParentConflicts {
				continue
			}
			return nil, 0, err
		}
		createdAt := item.UpdatedAt
		if createdAt.IsZero() {
			createdAt = item.CreatedAt
		}
		var linkID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO links(
from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,created_at
) VALUES($1,$2,(SELECT uid FROM issues WHERE id=$1),(SELECT uid FROM issues WHERE id=$2),$3,$4,$5)
RETURNING id`, fromID, toID, importLink.Type, params.Actor, formatStoredTime(createdAt)).Scan(&linkID)
		if err != nil {
			mapped := mapSQLError(err, linkConstraintErrors)
			if errors.Is(mapped, db.ErrParentAlreadySet) && params.PreserveLocalParentConflicts {
				continue
			}
			return nil, 0, mapped
		}
		if _, err := upsertImportMappingTx(ctx, tx, db.ImportMappingParams{
			Source: params.Source, ExternalID: externalID, ObjectType: "link",
			ProjectID: params.ProjectID, IssueID: &issue.ID, LinkID: &linkID,
			SourceUpdatedAt: &item.UpdatedAt,
		}); err != nil {
			return nil, 0, err
		}
		link, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE id=$1`, linkID))
		if err != nil {
			return nil, 0, err
		}
		event, err := s.insertImportLinkEvent(
			ctx, tx, params, issue, project, "issue.linked", link, item.UpdatedAt,
		)
		if err != nil {
			return nil, 0, err
		}
		events = append(events, event)
		created++
	}
	return events, created, nil
}

func (s *Store) insertImportLinkEvent(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	issue db.Issue,
	project db.Project,
	eventType string,
	link db.Link,
	updatedAt time.Time,
) (db.Event, error) {
	relatedID := link.ToIssueID
	if relatedID == issue.ID {
		relatedID = link.FromIssueID
	}
	toShortID, toUID, err := importIssueIdentByID(ctx, tx, relatedID)
	if err != nil {
		return db.Event{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"source": params.Source, "link_id": link.ID, "type": link.Type,
		"from_short_id": issue.ShortID, "from_uid": issue.UID,
		"to_short_id": toShortID, "to_uid": toUID,
		"updated_at": formatStoredTime(updatedAt),
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal link payload: %w", err)
	}
	return s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
		IssueID: &issue.ID, IssueUID: &issue.UID,
		RelatedIssueID: &relatedID, RelatedIssueUID: &toUID,
		Type: eventType, Actor: params.Actor, Payload: string(payload),
	})
}

func importIssueIdentByID(ctx context.Context, tx *sql.Tx, issueID int64) (string, string, error) {
	var shortID, issueUID string
	err := tx.QueryRowContext(ctx,
		`SELECT short_id,uid FROM issues WHERE id=$1`, issueID).Scan(&shortID, &issueUID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", db.ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("lookup issue ident: %w", mapSQLError(err, nil))
	}
	return shortID, issueUID, nil
}

func importLinkMappingMatches(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	issue db.Issue,
	importLink db.ImportLink,
	states map[string]*importIssueState,
	linkID int64,
) (bool, error) {
	fromID, toID, err := importLinkEndpoints(ctx, tx, params, issue, importLink, states)
	if err != nil {
		return false, err
	}
	link, err := scanLink(tx.QueryRowContext(ctx, linkSelect+` WHERE id=$1`, linkID))
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return link.FromIssueID == fromID && link.ToIssueID == toID && link.Type == importLink.Type, nil
}

func importLinkEndpoints(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	issue db.Issue,
	importLink db.ImportLink,
	states map[string]*importIssueState,
) (int64, int64, error) {
	target, err := resolveImportLinkTarget(ctx, tx, params, states, importLink.TargetExternalID)
	if err != nil {
		return 0, 0, err
	}
	fromID, toID := issue.ID, target.ID
	if importLink.Type == "related" && fromID > toID {
		fromID, toID = toID, fromID
	}
	return fromID, toID, nil
}

func resolveImportLinkTarget(
	ctx context.Context,
	tx *sql.Tx,
	params db.ImportBatchParams,
	states map[string]*importIssueState,
	externalID string,
) (db.Issue, error) {
	if state, ok := states[externalID]; ok {
		return state.issue, nil
	}
	mapping, err := importMappingBySourceTx(
		ctx, tx, params.ProjectID, params.Source, "issue", externalID,
	)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Issue{}, fmt.Errorf("%w: import link target %q", db.ErrNotFound, externalID)
		}
		return db.Issue{}, err
	}
	if mapping.IssueID == nil {
		return db.Issue{}, fmt.Errorf("%w: import link target %q", db.ErrNotFound, externalID)
	}
	issue, err := scanIssue(tx.QueryRowContext(ctx,
		issueSelect+` WHERE i.id=$1 AND i.deleted_at IS NULL`, *mapping.IssueID))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Issue{}, fmt.Errorf("%w: import link target %q", db.ErrNotFound, externalID)
		}
		return db.Issue{}, err
	}
	return issue, nil
}
