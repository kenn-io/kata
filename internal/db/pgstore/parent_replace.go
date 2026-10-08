package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

func (s *Store) ReplaceParentAndEvents(
	ctx context.Context,
	input db.ReplaceParentAndEventsParams,
) (db.ReplaceParentAndEventsResult, error) {
	var result db.ReplaceParentAndEventsResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result = db.ReplaceParentAndEventsResult{}
		if input.ExpectedParentLinkID <= 0 || input.ExpectedParentIssueID <= 0 ||
			input.Link.Type != "parent" || input.Link.FromIssueID <= 0 || input.Link.ToIssueID <= 0 ||
			input.Link.FromIssueID == input.Link.ToIssueID ||
			input.UnlinkEvent.EventIssueID != input.Link.FromIssueID ||
			input.LinkEvent.EventIssueID != input.Link.FromIssueID ||
			input.UnlinkEvent.EventType != "issue.unlinked" || input.LinkEvent.EventType != "issue.linked" {
			return fmt.Errorf("invalid parent replacement")
		}
		if err := lockProjectAccess(ctx, tx); err != nil {
			return err
		}
		if err := checkLinkEndpointsProjectAccessTx(ctx, tx,
			input.Link.FromIssueID, input.ExpectedParentIssueID, input.Link.ToIssueID,
		); err != nil {
			return err
		}
		if err := ensureRelayLinkBoundaryTx(ctx, tx, input.Link.FromIssueID, input.Link.ToIssueID); err != nil {
			return err
		}
		if err := requireAddableLinkTargetTx(ctx, tx, input.Link.ToIssueID); err != nil {
			return err
		}
		if err := assertNoParentCycleTx(ctx, tx, input.Link.FromIssueID, input.Link.ToIssueID); err != nil {
			return err
		}

		eventIssue, project, err := lockedIssueTx(ctx, tx, input.Link.FromIssueID, false)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtext(current_schema()), hashint8($1))`, input.Link.FromIssueID); err != nil {
			return mapSQLError(err, nil)
		}
		oldLink, err := scanLink(tx.QueryRowContext(ctx,
			linkSelect+` WHERE from_issue_id = $1 AND type = 'parent' FOR UPDATE`, input.Link.FromIssueID))
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrParentMismatch
		}
		if err != nil {
			return mapSQLError(err, nil)
		}
		if oldLink.ID == input.ExpectedParentLinkID && oldLink.ToIssueID == input.Link.ToIssueID {
			return db.ErrLinkExists
		}
		if oldLink.ID != input.ExpectedParentLinkID || oldLink.ToIssueID != input.ExpectedParentIssueID {
			return db.ErrParentMismatch
		}

		actor := input.LinkEvent.Actor
		if actor == "" {
			actor = input.Link.Author
		}
		actor, err = effectiveLocalMutationActorTx(ctx, tx, project.ID, actor)
		if err != nil {
			return err
		}
		input.Link.Author = actor
		input.UnlinkEvent.Actor = actor
		input.LinkEvent.Actor = actor

		deleted, err := tx.ExecContext(ctx,
			`DELETE FROM links WHERE id = $1 AND from_issue_id = $2 AND to_issue_id = $3 AND type = 'parent'`,
			oldLink.ID, input.Link.FromIssueID, input.ExpectedParentIssueID,
		)
		if err != nil {
			return mapSQLError(err, nil)
		}
		deletedRows, err := deleted.RowsAffected()
		if err != nil {
			return err
		}
		if deletedRows != 1 {
			return db.ErrParentMismatch
		}
		if err := prepareLinkInsertTx(ctx, tx, input.Link, true); err != nil {
			return err
		}
		result.Link, err = insertLinkTx(ctx, tx, input.Link)
		if err != nil {
			return err
		}

		updatedAt := nowStoredTimestamp()
		unlinkedPayload, err := json.Marshal(map[string]any{
			"link_id": oldLink.ID, "type": oldLink.Type,
			"from_short_id": input.UnlinkEvent.FromShortID, "from_uid": input.UnlinkEvent.FromUID,
			"to_short_id": input.UnlinkEvent.ToShortID, "to_uid": input.UnlinkEvent.ToUID,
			"updated_at": updatedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal unlink payload: %w", err)
		}
		oldRelatedID, oldRelatedUID := oldLink.ToIssueID, oldLink.ToIssueUID
		result.UnlinkedEvent, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			IssueID: &eventIssue.ID, IssueUID: &eventIssue.UID,
			RelatedIssueID: &oldRelatedID, RelatedIssueUID: &oldRelatedUID,
			Type: input.UnlinkEvent.EventType, Actor: actor, Payload: string(unlinkedPayload),
		})
		if err != nil {
			return err
		}
		linkedPayload, err := json.Marshal(map[string]any{
			"link_id": result.Link.ID, "type": result.Link.Type,
			"from_short_id": input.LinkEvent.FromShortID, "from_uid": input.LinkEvent.FromUID,
			"to_short_id": input.LinkEvent.ToShortID, "to_uid": input.LinkEvent.ToUID,
			"updated_at": updatedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal link payload: %w", err)
		}
		newRelatedID, newRelatedUID := result.Link.ToIssueID, result.Link.ToIssueUID
		result.LinkedEvent, err = s.insertEventTx(ctx, tx, eventInsert{
			ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
			IssueID: &eventIssue.ID, IssueUID: &eventIssue.UID,
			RelatedIssueID: &newRelatedID, RelatedIssueUID: &newRelatedUID,
			Type: input.LinkEvent.EventType, Actor: actor, Payload: string(linkedPayload),
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE issues SET updated_at = $1 WHERE id = $2`, updatedAt, eventIssue.ID); err != nil {
			return mapSQLError(err, nil)
		}
		return nil
	})
	return result, err
}

var _ db.ParentLinkReplacementStorage = (*Store)(nil)
