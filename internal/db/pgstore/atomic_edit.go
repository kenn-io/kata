package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// EditIssueAtomic applies scalar, priority, and relationship changes in one
// serializable transaction. A failure in any later relationship operation
// rolls back all earlier changes and events.
func (s *Store) EditIssueAtomic(ctx context.Context, params db.EditIssueAtomicParams) (db.EditIssueAtomicResult, error) {
	var result db.EditIssueAtomicResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result = db.EditIssueAtomicResult{}
		current, project, err := lockedIssueTx(ctx, tx, params.IssueID, false)
		if err != nil {
			return err
		}
		if err := ensureProjectWritableTx(ctx, tx, project.ID); err != nil {
			return err
		}
		params.Actor, err = effectiveLocalMutationActorTx(ctx, tx, project.ID, params.Actor)
		if err != nil {
			return err
		}
		updatedAt := mutationTimestamp()

		fieldPlan, err := db.PlanIssueFieldEdit(current, params.Title, params.Body, params.Owner, updatedAt)
		if err != nil {
			return err
		}
		if fieldPlan.Changed() {
			sets := make([]string, 0, 5)
			args := make([]any, 0, 6)
			if fieldPlan.TitleChanged {
				args = append(args, *params.Title)
				sets = append(sets, fmt.Sprintf("title = $%d", len(args)))
			}
			if fieldPlan.BodyChanged {
				args = append(args, *params.Body)
				sets = append(sets, fmt.Sprintf("body = $%d", len(args)))
			}
			if fieldPlan.OwnerChanged {
				args = append(args, fieldPlan.Owner)
				sets = append(sets, fmt.Sprintf("owner = $%d", len(args)))
			}
			args = append(args, updatedAt)
			sets = append(sets, fmt.Sprintf("updated_at = $%d", len(args)))
			if fieldPlan.ContentChanged() {
				sets = append(sets, "content_revision = content_revision + 1")
			}
			args = append(args, current.ID)
			query := `UPDATE issues SET ` + strings.Join(sets, ", ") + fmt.Sprintf(` WHERE id = $%d`, len(args)) // #nosec G202 -- fragments are fixed literals
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return mapSQLError(err, nil)
			}
			event, err := s.insertEventTx(ctx, tx,
				issueEventInput(current, project, "issue.updated", params.Actor, fieldPlan.Payload))
			if err != nil {
				return err
			}
			result.Events = append(result.Events, event)
			result.AnyChange = true
		}

		if params.SetPriority != nil || params.ClearPriority {
			var next *int64
			if !params.ClearPriority {
				next = params.SetPriority
			}
			if !equalIntPointers(current.Priority, next) {
				if _, err := tx.ExecContext(ctx,
					`UPDATE issues SET priority = $1, updated_at = $2 WHERE id = $3`,
					next, updatedAt, current.ID); err != nil {
					return mapSQLError(err, nil)
				}
				eventType, payload, err := db.PriorityEventPayload(current.Priority, next, updatedAt)
				if err != nil {
					return err
				}
				event, err := s.insertEventTx(ctx, tx,
					issueEventInput(current, project, eventType, params.Actor, payload))
				if err != nil {
					return err
				}
				result.Events = append(result.Events, event)
				result.AnyChange = true
			}
		}

		linksChanged, err := s.applyAtomicLinkDeltaTx(ctx, tx, current, params, &result.Changes, updatedAt)
		if err != nil {
			return err
		}
		if linksChanged {
			payload, err := db.LinksChangedPayload(result.Changes, updatedAt)
			if err != nil {
				return fmt.Errorf("marshal links_changed payload: %w", err)
			}
			peerID, peerUID, err := atomicSinglePeerTx(ctx, tx, result.Changes)
			if err != nil {
				return err
			}
			event, err := s.insertEventTx(ctx, tx, eventInsert{
				ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
				IssueID: &current.ID, IssueUID: &current.UID,
				RelatedIssueID: peerID, RelatedIssueUID: peerUID,
				Type: "issue.links_changed", Actor: params.Actor, Payload: string(payload),
			})
			if err != nil {
				return err
			}
			result.Events = append(result.Events, event)
			result.AnyChange = true
		}

		result.Issue, err = scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = $1`, current.ID))
		return err
	})
	return result, err
}

func (s *Store) applyAtomicLinkDeltaTx(
	ctx context.Context,
	tx *sql.Tx,
	issue db.Issue,
	params db.EditIssueAtomicParams,
	changes *db.AtomicEditChanges,
	updatedAt string,
) (bool, error) {
	changed := false
	if params.SetParent != nil || params.RemoveParent != nil {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtext(current_schema()), hashint8($1))`, issue.ID); err != nil {
			return changed, mapSQLError(err, nil)
		}
	}
	if params.SetParent != nil {
		target, err := atomicIssueByIDTx(ctx, tx, *params.SetParent, false)
		if errors.Is(err, db.ErrNotFound) {
			return changed, &db.LinkTargetNotFoundError{Number: *params.SetParent}
		}
		if err != nil {
			return changed, err
		}
		if target.ID == issue.ID {
			return changed, db.ErrSelfLink
		}
		if err := assertNoParentCycleTx(ctx, tx, issue.ID, target.ID); err != nil {
			return changed, err
		}
		existing, err := atomicLinkByEndpointsTx(ctx, tx, issue.ID, 0, "parent", true)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return changed, err
		}
		if errors.Is(err, db.ErrNotFound) || existing.ToIssueID != target.ID {
			if err == nil {
				identity, identityErr := atomicPeerIdentityTx(ctx, tx, existing.ToIssueID)
				if identityErr != nil {
					return changed, identityErr
				}
				deleted, deleteErr := deleteAtomicLinkTx(ctx, tx, existing.ID)
				if deleteErr != nil {
					return changed, deleteErr
				}
				if deleted {
					changes.ParentRemoved = &identity
					changed = true
				}
			}
			_, insertErr := insertLinkTx(ctx, tx, db.CreateLinkParams{
				FromIssueID: issue.ID, ToIssueID: target.ID, Type: "parent", Author: params.Actor,
			})
			if insertErr != nil && !errors.Is(insertErr, db.ErrLinkExists) {
				return changed, insertErr
			}
			if insertErr == nil {
				identity, identityErr := atomicPeerIdentityTx(ctx, tx, target.ID)
				if identityErr != nil {
					return changed, identityErr
				}
				changes.ParentSet = &identity
				changed = true
			}
		}
	}

	if params.RemoveParent != nil {
		existing, err := atomicLinkByEndpointsTx(ctx, tx, issue.ID, 0, "parent", true)
		if errors.Is(err, db.ErrNotFound) {
			return changed, db.ErrParentMismatch
		}
		if err != nil {
			return changed, err
		}
		if existing.ToIssueID != *params.RemoveParent {
			return changed, db.ErrParentMismatch
		}
		identity, err := atomicPeerIdentityTx(ctx, tx, existing.ToIssueID)
		if err != nil {
			return changed, err
		}
		deleted, err := deleteAtomicLinkTx(ctx, tx, existing.ID)
		if err != nil {
			return changed, err
		}
		if !deleted {
			return changed, db.ErrParentMismatch
		}
		changes.ParentRemoved = &identity
		changed = true
	}

	for _, targetID := range params.AddBlocks {
		added, peer, err := atomicAddEdgeTx(ctx, tx, issue, targetID, "blocks", params.Actor, false)
		if err != nil {
			return changed, err
		}
		if added {
			changes.BlocksAdded = append(changes.BlocksAdded, peer)
			changed = true
		}
	}
	for _, targetID := range params.AddBlockedBy {
		added, peer, err := atomicAddEdgeTx(ctx, tx, issue, targetID, "blocks", params.Actor, true)
		if err != nil {
			return changed, err
		}
		if added {
			changes.BlockedByAdded = append(changes.BlockedByAdded, peer)
			changed = true
		}
	}
	for _, targetID := range params.AddRelated {
		added, peer, err := atomicAddEdgeTx(ctx, tx, issue, targetID, "related", params.Actor, false)
		if err != nil {
			return changed, err
		}
		if added {
			changes.RelatedAdded = append(changes.RelatedAdded, peer)
			changed = true
		}
	}
	for _, targetID := range params.RemoveBlocks {
		removed, peer, err := atomicRemoveEdgeTx(ctx, tx, issue, targetID, "blocks", false)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.BlocksRemoved = append(changes.BlocksRemoved, peer)
			changed = true
		}
	}
	for _, targetID := range params.RemoveBlockedBy {
		removed, peer, err := atomicRemoveEdgeTx(ctx, tx, issue, targetID, "blocks", true)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.BlockedByRemoved = append(changes.BlockedByRemoved, peer)
			changed = true
		}
	}
	for _, targetID := range params.RemoveRelated {
		removed, peer, err := atomicRemoveEdgeTx(ctx, tx, issue, targetID, "related", false)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.RelatedRemoved = append(changes.RelatedRemoved, peer)
			changed = true
		}
	}
	if changed {
		_, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at = $1 WHERE id = $2`, updatedAt, issue.ID)
		return true, mapSQLError(err, nil)
	}
	return false, nil
}

func atomicAddEdgeTx(
	ctx context.Context,
	tx *sql.Tx,
	issue db.Issue,
	targetID int64,
	linkType string,
	actor string,
	reverse bool,
) (bool, db.PeerIdentity, error) {
	target, err := atomicIssueByIDTx(ctx, tx, targetID, false)
	if errors.Is(err, db.ErrNotFound) {
		return false, db.PeerIdentity{}, &db.LinkTargetNotFoundError{Number: targetID}
	}
	if err != nil {
		return false, db.PeerIdentity{}, err
	}
	if target.ID == issue.ID {
		return false, db.PeerIdentity{}, db.ErrSelfLink
	}
	fromID, toID := issue.ID, target.ID
	if reverse {
		fromID, toID = toID, fromID
	}
	if linkType == "related" && fromID > toID {
		fromID, toID = toID, fromID
	}
	_, err = atomicLinkByEndpointsTx(ctx, tx, fromID, toID, linkType, false)
	if err == nil {
		return false, db.PeerIdentity{}, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return false, db.PeerIdentity{}, err
	}
	inserted, err := insertAtomicEdgeTx(ctx, tx, db.CreateLinkParams{
		FromIssueID: fromID, ToIssueID: toID, Type: linkType, Author: actor,
	})
	if err != nil {
		return false, db.PeerIdentity{}, err
	}
	if !inserted {
		return false, db.PeerIdentity{}, nil
	}
	peer, err := atomicPeerIdentityTx(ctx, tx, target.ID)
	return true, peer, err
}

func insertAtomicEdgeTx(ctx context.Context, tx *sql.Tx, params db.CreateLinkParams) (bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO links(
  from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author
)
SELECT $1, $2, source.uid, target.uid, $3, $4
  FROM issues source, issues target
 WHERE source.id = $1 AND target.id = $2
ON CONFLICT DO NOTHING
RETURNING id`, params.FromIssueID, params.ToIssueID, params.Type, params.Author).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, mapSQLError(err, linkConstraintErrors)
}

func atomicRemoveEdgeTx(
	ctx context.Context,
	tx *sql.Tx,
	issue db.Issue,
	targetID int64,
	linkType string,
	reverse bool,
) (bool, db.PeerIdentity, error) {
	target, err := atomicIssueByIDTx(ctx, tx, targetID, true)
	if errors.Is(err, db.ErrNotFound) {
		return false, db.PeerIdentity{}, nil
	}
	if err != nil {
		return false, db.PeerIdentity{}, err
	}
	fromID, toID := issue.ID, target.ID
	if reverse {
		fromID, toID = toID, fromID
	}
	if linkType == "related" && fromID > toID {
		fromID, toID = toID, fromID
	}
	link, err := atomicLinkByEndpointsTx(ctx, tx, fromID, toID, linkType, false)
	if errors.Is(err, db.ErrNotFound) {
		return false, db.PeerIdentity{}, nil
	}
	if err != nil {
		return false, db.PeerIdentity{}, err
	}
	deleted, err := deleteAtomicLinkTx(ctx, tx, link.ID)
	if err != nil || !deleted {
		return false, db.PeerIdentity{}, err
	}
	peer, err := atomicPeerIdentityTx(ctx, tx, target.ID)
	return true, peer, err
}

func atomicIssueByIDTx(ctx context.Context, tx *sql.Tx, issueID int64, includeDeleted bool) (db.Issue, error) {
	query := issueSelect + ` WHERE i.id = $1`
	if !includeDeleted {
		query += ` AND i.deleted_at IS NULL`
	}
	return scanIssue(tx.QueryRowContext(ctx, query, issueID))
}

func atomicLinkByEndpointsTx(
	ctx context.Context,
	tx *sql.Tx,
	fromID int64,
	toID int64,
	linkType string,
	anyTarget bool,
) (db.Link, error) {
	if anyTarget {
		return scanLink(tx.QueryRowContext(ctx,
			linkSelect+` WHERE from_issue_id = $1 AND type = $2`, fromID, linkType))
	}
	return scanLink(tx.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = $1 AND to_issue_id = $2 AND type = $3`,
		fromID, toID, linkType))
}

func deleteAtomicLinkTx(ctx context.Context, tx *sql.Tx, linkID int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = $1`, linkID)
	if err != nil {
		return false, mapSQLError(err, nil)
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func atomicPeerIdentityTx(ctx context.Context, tx *sql.Tx, issueID int64) (db.PeerIdentity, error) {
	var peer db.PeerIdentity
	err := tx.QueryRowContext(ctx, `SELECT i.short_id, i.uid, p.name, i.status
FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = $1`, issueID).
		Scan(&peer.ShortID, &peer.UID, &peer.Project, &peer.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PeerIdentity{}, db.ErrNotFound
	}
	return peer, mapSQLError(err, nil)
}

func atomicSinglePeerTx(
	ctx context.Context,
	tx *sql.Tx,
	changes db.AtomicEditChanges,
) (*int64, *string, error) {
	seen := make(map[string]struct{})
	add := func(peer *db.PeerIdentity) {
		if peer != nil && peer.UID != "" {
			seen[peer.UID] = struct{}{}
		}
	}
	add(changes.ParentSet)
	add(changes.ParentRemoved)
	for _, peers := range [][]db.PeerIdentity{
		changes.BlocksAdded, changes.BlocksRemoved,
		changes.BlockedByAdded, changes.BlockedByRemoved,
		changes.RelatedAdded, changes.RelatedRemoved,
	} {
		for index := range peers {
			add(&peers[index])
		}
	}
	if len(seen) != 1 {
		return nil, nil, nil
	}
	var peerUID string
	for value := range seen {
		peerUID = value
	}
	var peerID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid = $1`, peerUID).Scan(&peerID); err != nil {
		return nil, nil, mapSQLError(err, nil)
	}
	return &peerID, &peerUID, nil
}
