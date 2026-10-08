package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) ReplaceParentAndEvents(
	ctx context.Context,
	input db.ReplaceParentAndEventsParams,
) (db.ReplaceParentAndEventsResult, error) {
	return retryWrite1(ctx, d, func() (db.ReplaceParentAndEventsResult, error) {
		return d.replaceParentAndEventsTx(ctx, input)
	})
}

func (d *Store) replaceParentAndEventsTx(
	ctx context.Context,
	input db.ReplaceParentAndEventsParams,
) (db.ReplaceParentAndEventsResult, error) {
	var result db.ReplaceParentAndEventsResult
	if input.ExpectedParentLinkID <= 0 || input.ExpectedParentIssueID <= 0 ||
		input.Link.Type != "parent" || input.Link.FromIssueID <= 0 || input.Link.ToIssueID <= 0 ||
		input.Link.FromIssueID == input.Link.ToIssueID ||
		input.UnlinkEvent.EventIssueID != input.Link.FromIssueID ||
		input.LinkEvent.EventIssueID != input.Link.FromIssueID ||
		input.UnlinkEvent.EventType != "issue.unlinked" || input.LinkEvent.EventType != "issue.linked" {
		return result, fmt.Errorf("invalid parent replacement")
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockProjectAccess(ctx, tx); err != nil {
		return result, err
	}
	if err := checkLinkEndpointsProjectAccessTx(ctx, tx,
		input.Link.FromIssueID, input.ExpectedParentIssueID, input.Link.ToIssueID,
	); err != nil {
		return result, err
	}
	if err := ensureRelayLinkBoundaryTx(ctx, tx, input.Link.FromIssueID, input.Link.ToIssueID); err != nil {
		return result, err
	}
	if err := requireAddableLinkTargetTx(ctx, tx, input.Link.ToIssueID); err != nil {
		return result, err
	}
	if err := assertNoParentCycleTx(ctx, tx, input.Link.FromIssueID, input.Link.ToIssueID); err != nil {
		return result, err
	}

	oldLink, err := scanLink(tx.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND type = 'parent'`, input.Link.FromIssueID))
	if errors.Is(err, sql.ErrNoRows) {
		return result, db.ErrParentMismatch
	}
	if err != nil {
		return result, err
	}
	if oldLink.ID == input.ExpectedParentLinkID && oldLink.ToIssueID == input.Link.ToIssueID {
		return result, db.ErrLinkExists
	}
	if oldLink.ID != input.ExpectedParentLinkID || oldLink.ToIssueID != input.ExpectedParentIssueID {
		return result, db.ErrParentMismatch
	}

	eventIssue, projectName, err := lookupIssueForEvent(ctx, tx, input.Link.FromIssueID)
	if err != nil {
		return result, err
	}
	actor := input.LinkEvent.Actor
	if actor == "" {
		actor = input.Link.Author
	}
	actor, err = d.effectiveLocalMutationActorTx(ctx, tx, eventIssue.ProjectID, actor)
	if err != nil {
		return result, err
	}
	input.Link.Author = actor
	input.UnlinkEvent.Actor = actor
	input.LinkEvent.Actor = actor

	deleteResult, err := tx.ExecContext(ctx,
		`DELETE FROM links WHERE id = ? AND from_issue_id = ? AND to_issue_id = ? AND type = 'parent'`,
		oldLink.ID, input.Link.FromIssueID, input.ExpectedParentIssueID,
	)
	if err != nil {
		return result, fmt.Errorf("delete old parent link: %w", err)
	}
	deleted, err := deleteResult.RowsAffected()
	if err != nil {
		return result, err
	}
	if deleted != 1 {
		return result, db.ErrParentMismatch
	}
	if err := insertLinkRowTx(ctx, tx, input.Link.FromIssueID, input.Link.ToIssueID, "parent", actor); err != nil {
		return result, err
	}
	result.Link, err = scanLink(tx.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND to_issue_id = ? AND type = 'parent'`,
		input.Link.FromIssueID, input.Link.ToIssueID,
	))
	if err != nil {
		return result, fmt.Errorf("re-fetch replacement parent inside tx: %w", err)
	}

	updatedAt := nowTimestamp()
	unlinkedPayload, err := json.Marshal(map[string]any{
		"link_id": oldLink.ID, "type": oldLink.Type,
		"from_short_id": input.UnlinkEvent.FromShortID, "from_uid": input.UnlinkEvent.FromUID,
		"to_short_id": input.UnlinkEvent.ToShortID, "to_uid": input.UnlinkEvent.ToUID,
		"updated_at": updatedAt,
	})
	if err != nil {
		return result, fmt.Errorf("marshal unlink payload: %w", err)
	}
	oldRelatedID, oldRelatedUID := oldLink.ToIssueID, oldLink.ToIssueUID
	result.UnlinkedEvent, err = d.insertEventTx(ctx, tx, eventInsert{
		ProjectID: eventIssue.ProjectID, ProjectName: projectName,
		IssueID: &eventIssue.ID, IssueUID: &eventIssue.UID,
		RelatedIssueID: &oldRelatedID, RelatedIssueUID: &oldRelatedUID,
		Type: input.UnlinkEvent.EventType, Actor: actor, Payload: string(unlinkedPayload),
	})
	if err != nil {
		return result, err
	}
	linkedPayload, err := json.Marshal(map[string]any{
		"link_id": result.Link.ID, "type": result.Link.Type,
		"from_short_id": input.LinkEvent.FromShortID, "from_uid": input.LinkEvent.FromUID,
		"to_short_id": input.LinkEvent.ToShortID, "to_uid": input.LinkEvent.ToUID,
		"updated_at": updatedAt,
	})
	if err != nil {
		return result, fmt.Errorf("marshal link payload: %w", err)
	}
	newRelatedID, newRelatedUID := result.Link.ToIssueID, result.Link.ToIssueUID
	result.LinkedEvent, err = d.insertEventTx(ctx, tx, eventInsert{
		ProjectID: eventIssue.ProjectID, ProjectName: projectName,
		IssueID: &eventIssue.ID, IssueUID: &eventIssue.UID,
		RelatedIssueID: &newRelatedID, RelatedIssueUID: &newRelatedUID,
		Type: input.LinkEvent.EventType, Actor: actor, Payload: string(linkedPayload),
	})
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at = ? WHERE id = ?`, updatedAt, eventIssue.ID); err != nil {
		return result, fmt.Errorf("touch replacement issue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit parent replacement: %w", err)
	}
	return result, nil
}

var _ db.ParentLinkReplacementStorage = (*Store)(nil)
