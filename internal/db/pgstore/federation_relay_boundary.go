package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/db"
)

func ensureRelayLinkBoundaryTx(ctx context.Context, tx *sql.Tx, fromID, toID int64, cleanup bool) error {
	var fromProject, toProject int64
	err := tx.QueryRowContext(ctx, `SELECT f.project_id,t.project_id FROM issues f JOIN issues t ON t.id=$1 WHERE f.id=$2`, toID, fromID).Scan(&fromProject, &toProject)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} // Preserve native missing-target classification.
	if err != nil {
		return err
	}
	if fromProject == toProject {
		return nil
	}
	var relayed bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_bindings b WHERE b.project_id IN($1,$2) AND (b.relay_config IS NOT NULL OR EXISTS(SELECT 1 FROM federation_enrollments e WHERE e.project_id=b.project_id AND e.relay_protocol_version=1 AND e.revoked_at IS NULL)))`, fromProject, toProject).Scan(&relayed)
	if err != nil {
		return err
	}
	if relayed {
		compatible, err := relaySharedProjectsTx(ctx, tx, fromProject, toProject, true, cleanup)
		if err != nil {
			return err
		}
		if compatible {
			return nil
		}
		return errors.Join(db.ErrFederatedReadOnly, db.ErrFederationCrossProjectBoundary)
	}
	return nil
}

// CheckRelayProjectLinks uses the same native boundary check as enrollment.
// Check visibility before reading links, including links to hidden projects.
func (d *Store) CheckRelayProjectLinks(ctx context.Context, projectID int64) error {
	// Native group discovery takes row locks but never changes preflight state.
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 AND deleted_at IS NULL`, projectID).Scan(&projectUID); err != nil {
		return db.RelayProjectLookupError(err)
	}
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return db.ErrNotFound
	}
	return rejectRelayProjectLinksTx(ctx, tx, projectID)
}

// Enrollment admits crossings only inside the existing common-root group.
func rejectRelayProjectLinksTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT CASE WHEN f.project_id=$1 THEN t.project_id ELSE f.project_id END FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id WHERE f.project_id<>t.project_id AND (f.project_id=$2 OR t.project_id=$3)`, projectID, projectID, projectID)
	if err != nil {
		return err
	}
	var peers []int64
	for rows.Next() {
		var peer int64
		if err = rows.Scan(&peer); err != nil {
			break
		}
		peers = append(peers, peer)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, peer := range peers {
		compatible, err := relaySharedProjectsTx(ctx, tx, projectID, peer, false, false)
		if err != nil {
			return err
		}
		if !compatible {
			return errors.Join(db.ErrFederatedReadOnly, db.ErrFederationCrossProjectBoundary)
		}
	}
	return nil
}

// relaySharedProjectsTx reuses the native binding group and pinned authority.
// Local relationship writes require both endpoints to be authoritative here.
func relaySharedProjectsTx(ctx context.Context, tx *sql.Tx, projectID, peerID int64, localWrite, cleanup bool) (bool, error) {
	return relaySharedProjectsWithGroupLockTx(ctx, tx, projectID, peerID, localWrite, cleanup, true)
}

func relaySharedProjectsReadOnlyTx(ctx context.Context, tx *sql.Tx, projectID, peerID int64, localWrite, cleanup bool) (bool, error) {
	return relaySharedProjectsWithGroupLockTx(ctx, tx, projectID, peerID, localWrite, cleanup, false)
}

func relaySharedProjectsWithGroupLockTx(ctx context.Context, tx *sql.Tx, projectID, peerID int64, localWrite, cleanup, lockGroup bool) (bool, error) {
	if projectID == peerID {
		return true, nil
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, projectID))
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !binding.Enabled || (localWrite && binding.Role != db.FederationRoleHub) {
		return false, nil
	}
	var group []int64
	if lockGroup {
		group, err = federationBindingGroupProjectIDs(ctx, tx, binding)
	} else {
		group, err = federationBindingGroupProjectIDsReadOnly(ctx, tx, binding)
	}
	if err != nil {
		return false, err
	}
	if !slices.Contains(group, peerID) {
		return false, nil
	}
	authority, err := relayRootAuthorityTx(ctx, tx, projectID, cleanup)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	peer, err := relayRootAuthorityTx(ctx, tx, peerID, cleanup)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if authority != peer {
		return false, nil
	}
	if localWrite {
		var instance string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&instance); err != nil {
			return false, err
		}
		return authority == instance, nil
	}
	return true, nil
}

// Pins take precedence; an enabled native hub is already rooted locally
// before its first handshake initializes the public signing pin.
func relayRootAuthorityTx(ctx context.Context, tx *sql.Tx, projectID int64, cleanup bool) (string, error) {
	var projectUID, role, instance string
	err := tx.QueryRowContext(ctx, `SELECT p.uid,b.role,m.value FROM projects p JOIN federation_bindings b ON b.project_id=p.id JOIN meta m ON m.key='instance_uid' WHERE p.id=$1 AND ($2 OR p.deleted_at IS NULL) AND b.enabled=1`, projectID, cleanup).Scan(&projectUID, &role, &instance)
	if errors.Is(err, sql.ErrNoRows) {
		return "", db.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	pin, err := rootPinTx(ctx, tx, projectUID, "")
	if !errors.Is(err, db.ErrNotFound) {
		return pin.AuthorityUID, err
	}
	if role == string(db.FederationRoleHub) {
		return instance, nil
	}
	return "", db.ErrNotFound
}

// Root-authored shared references retain their original history and hashes.
func relayEventCrossesProjectTx(ctx context.Context, tx *sql.Tx, projectID int64, event db.RemoteEvent) (bool, error) {
	crossing, _, err := relayEventBoundaryFactsTx(ctx, tx, projectID, event, false, true)
	return crossing, err
}

// Fresh checkpoints cannot carry root-local purge facts. Exclude a history
// record that needed one to resolve its references, while retaining the strict
// resolver used for ordinary event authorization and retained checkpoints.
func relayEventCheckpointHistoryExcludedTx(ctx context.Context, tx *sql.Tx, projectID int64, event db.RemoteEvent, lockGroup bool) (bool, error) {
	crossing, usedPurgeFact, err := relayEventBoundaryFactsTx(ctx, tx, projectID, event, true, lockGroup)
	return crossing || usedPurgeFact, err
}

func relayEventBoundaryFactsTx(ctx context.Context, tx *sql.Tx, projectID int64, event db.RemoteEvent, trackPurgeFacts, lockGroup bool) (bool, bool, error) {
	var projects []int64
	var usedPurgeFact bool
	var err error
	if trackPurgeFacts {
		projects, usedPurgeFact, err = relayEventProjectsWithPurgeFactsTx(ctx, tx, event)
	} else {
		projects, err = relayEventProjectsTx(ctx, tx, event)
	}
	if errors.Is(err, db.ErrNotFound) {
		return true, usedPurgeFact, nil
	}
	if err != nil {
		return false, usedPurgeFact, err
	}
	for _, peer := range projects {
		if peer == projectID {
			continue
		}
		// Only relationship mutations require root authorship. Ordinary close
		// freezes references whose projects are still authorized at release.
		if db.FederationEventAffectsLinks(event.Type) {
			authority, err := relayRootAuthorityTx(ctx, tx, projectID, false)
			if errors.Is(err, db.ErrNotFound) {
				return true, usedPurgeFact, nil
			}
			if err != nil {
				return false, usedPurgeFact, err
			}
			if event.OriginInstanceUID != authority {
				return true, usedPurgeFact, nil
			}
		}
		var compatible bool
		if lockGroup {
			compatible, err = relaySharedProjectsTx(ctx, tx, projectID, peer, false, false)
		} else {
			compatible, err = relaySharedProjectsReadOnlyTx(ctx, tx, projectID, peer, false, false)
		}
		if err != nil || !compatible {
			return !compatible, usedPurgeFact, err
		}
	}
	return false, usedPurgeFact, nil
}

func relayHistoryHasBoundaryTx(ctx context.Context, tx *sql.Tx, projectID int64) (bool, error) {
	return relayHistoryHasBoundaryAfterTx(ctx, tx, projectID, 0)
}

func relayHistoryHasBoundaryAfterTx(ctx context.Context, tx *sql.Tx, projectID, after int64) (bool, error) {
	for {
		// #nosec G202 -- The event-type predicate is fixed native SQL; all project and cursor values remain bound.
		rows, err := tx.QueryContext(ctx, `SELECT e.id FROM events e WHERE e.project_id=$1 AND e.id>$2 AND `+pgFederationPushEventTypeCondition("e.type")+` ORDER BY e.id LIMIT 100`, projectID, after)
		if err != nil {
			return false, err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return false, err
		}
		if len(ids) == 0 {
			return false, nil
		}
		for _, id := range ids {
			event, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=$1`, id))
			if err != nil {
				return false, err
			}
			crossing, err := relayEventCrossesProjectTx(ctx, tx, projectID, db.RemoteEventFromStored(event))
			if err != nil || crossing {
				return crossing, err
			}
		}
		after = ids[len(ids)-1]
	}
}

func relayEventProjectsTx(ctx context.Context, tx *sql.Tx, event db.RemoteEvent) ([]int64, error) {
	ctx = db.WithAuthorizedProjects(ctx, nil)
	refs, err := payloadReferencedIssueUIDs(event, db.PayloadMap(event.Payload))
	if err != nil {
		return nil, err
	}
	return db.RelayEventReferencedProjectsTx(ctx, tx, event, refs,
		func(uid string) (db.Issue, error) { return eventIssueByUID(ctx, tx, uid) },
		func(n int) string { return fmt.Sprintf("$%d", n) }, func(field string) string { return "(e.payload::jsonb->>'" + field + "')" })
}

func relayEventProjectsWithPurgeFactsTx(ctx context.Context, tx *sql.Tx, event db.RemoteEvent) ([]int64, bool, error) {
	projects, _, usedPurgeFact, err := relayEventProjectsWithShortIDReferencesTx(ctx, tx, event)
	return projects, usedPurgeFact, err
}

func relayEventShortIDReferencesTx(ctx context.Context, tx *sql.Tx, event db.RemoteEvent) ([]db.RelayShortIDReference, error) {
	_, references, _, err := relayEventProjectsWithShortIDReferencesTx(ctx, tx, event)
	return references, err
}

func relayEventProjectsWithShortIDReferencesTx(ctx context.Context, tx *sql.Tx, event db.RemoteEvent) ([]int64, []db.RelayShortIDReference, bool, error) {
	ctx = db.WithAuthorizedProjects(ctx, nil)
	refs, err := payloadReferencedIssueUIDs(event, db.PayloadMap(event.Payload))
	if err != nil {
		return nil, nil, false, err
	}
	return db.RelayEventReferencedProjectsWithShortIDReferencesTx(ctx, tx, event, refs,
		func(uid string) (db.Issue, error) { return eventIssueByUID(ctx, tx, uid) },
		func(n int) string { return fmt.Sprintf("$%d", n) }, func(field string) string { return "(e.payload::jsonb->>'" + field + "')" },
		func(uid string) (bool, error) {
			var purged bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM purge_log WHERE issue_uid=$1)`, uid).Scan(&purged)
			return purged, err
		})
}

// A fresh checkpoint may omit a dangling reply edge only after the retained
// reply event proves the target's owner is still in the same shared group.
func relayCheckpointCanOmitPurgedCommentReferenceTx(ctx context.Context, tx *sql.Tx, sourceProjectID int64, sourceIssueUID, sourceCommentUID, targetCommentUID string, lockGroup bool) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM comments WHERE uid=$1)`, targetCommentUID).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	query := `SELECT COUNT(DISTINCT p.project_id),COALESCE(MIN(p.project_id),0)
		FROM events e JOIN purge_log p ON p.issue_uid=e.related_issue_uid
		WHERE e.type='issue.commented' AND e.related_issue_uid IS NOT NULL AND e.related_issue_uid<>''
		AND (e.payload::jsonb->>'reply_to_uid')=$1`
	args := []any{targetCommentUID}
	if sourceIssueUID != "" {
		query += ` AND e.issue_uid=$2 AND (e.payload::jsonb->>'comment_uid')=$3`
		args = append(args, sourceIssueUID, sourceCommentUID)
	}
	var owners int
	var targetProjectID int64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&owners, &targetProjectID); err != nil {
		return false, err
	}
	if owners != 1 {
		return false, nil
	}
	if targetProjectID == sourceProjectID {
		return true, nil
	}
	if lockGroup {
		return relaySharedProjectsTx(ctx, tx, sourceProjectID, targetProjectID, false, false)
	}
	return relaySharedProjectsReadOnlyTx(ctx, tx, sourceProjectID, targetProjectID, false, false)
}

func (d *Store) authorizeRelayEventTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment, event db.RemoteEvent) error {
	projects, err := relayEventProjectsTx(ctx, tx, event)
	if err != nil {
		return err
	}
	for _, projectID := range projects {
		var projectUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 AND deleted_at IS NULL`, projectID).Scan(&projectUID); err != nil {
			return db.RelayProjectLookupError(err)
		}
		if projectID != *grant.ProjectID {
			if err := d.ProjectAccessTransactionFence(grant.Actor, []string{projectUID})(ctx, tx); err != nil {
				return err
			}
		}
		if err := db.CheckProjectAccessTransaction(ctx, tx, projectUID); err != nil {
			return err
		}
	}
	return nil
}
