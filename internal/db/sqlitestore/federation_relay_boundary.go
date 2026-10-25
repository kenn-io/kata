package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/kata/internal/db"
)

func ensureRelayLinkBoundaryTx(ctx context.Context, tx *sql.Tx, fromID, toID int64) error {
	var fromProject, toProject int64
	err := tx.QueryRowContext(ctx, `SELECT f.project_id,t.project_id FROM issues f JOIN issues t ON t.id=? WHERE f.id=?`, toID, fromID).Scan(&fromProject, &toProject)
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
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_bindings b WHERE b.project_id IN(?,?) AND (b.relay_config IS NOT NULL OR EXISTS(SELECT 1 FROM federation_enrollments e WHERE e.project_id=b.project_id AND e.relay_protocol_version=1 AND e.revoked_at IS NULL)))`, fromProject, toProject).Scan(&relayed)
	if err != nil {
		return err
	}
	if relayed {
		compatible, err := relaySharedProjectsTx(ctx, tx, fromProject, toProject, true)
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
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=? AND deleted_at IS NULL`, projectID).Scan(&projectUID); err != nil {
		return db.RelayProjectLookupError(err)
	}
	if !db.ProjectAttributionVisible(ctx, projectUID) {
		return db.ErrNotFound
	}
	return rejectRelayProjectLinksTx(ctx, tx, projectID)
}

// Enrollment admits crossings only inside the existing common-root group.
func rejectRelayProjectLinksTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT CASE WHEN f.project_id=? THEN t.project_id ELSE f.project_id END FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id WHERE f.project_id<>t.project_id AND (f.project_id=? OR t.project_id=?)`, projectID, projectID, projectID)
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
		compatible, err := relaySharedProjectsTx(ctx, tx, projectID, peer, false)
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
func relaySharedProjectsTx(ctx context.Context, tx *sql.Tx, projectID, peerID int64, localWrite bool) (bool, error) {
	if projectID == peerID {
		return true, nil
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=?`, projectID))
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !binding.Enabled || (localWrite && binding.Role != db.FederationRoleHub) {
		return false, nil
	}
	group, err := federationBindingGroupProjectIDs(ctx, tx, binding)
	if err != nil {
		return false, err
	}
	if !slices.Contains(group, peerID) {
		return false, nil
	}
	var projectUID, peerUID string
	if err := tx.QueryRowContext(ctx, `SELECT a.uid,b.uid FROM projects a JOIN projects b ON b.id=? WHERE a.id=? AND a.deleted_at IS NULL AND b.deleted_at IS NULL`, peerID, projectID).Scan(&projectUID, &peerUID); err != nil {
		return false, err
	}
	pin, err := rootPinTx(ctx, tx, projectUID, "")
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	peer, err := rootPinTx(ctx, tx, peerUID, "")
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pin.AuthorityUID != peer.AuthorityUID {
		return false, nil
	}
	if localWrite {
		var instance string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&instance); err != nil {
			return false, err
		}
		return pin.AuthorityUID == instance, nil
	}
	return true, nil
}

// Root-authored shared references retain their original history and hashes.
func relayEventCrossesProjectTx(ctx context.Context, tx *sql.Tx, projectID int64, event db.RemoteEvent) (bool, error) {
	projects, err := relayEventProjectsTx(ctx, tx, event)
	if errors.Is(err, db.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, peer := range projects {
		if peer == projectID {
			continue
		}
		pin, err := rootPinTx(ctx, tx, event.ProjectUID, "")
		if errors.Is(err, db.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if event.OriginInstanceUID != pin.AuthorityUID {
			return true, nil
		}
		compatible, err := relaySharedProjectsTx(ctx, tx, projectID, peer, false)
		if err != nil || !compatible {
			return !compatible, err
		}
	}
	return false, nil
}

func relayHistoryHasBoundaryTx(ctx context.Context, tx *sql.Tx, projectID int64) (bool, error) {
	return relayHistoryHasBoundaryAfterTx(ctx, tx, projectID, 0)
}

func relayHistoryHasBoundaryAfterTx(ctx context.Context, tx *sql.Tx, projectID, after int64) (bool, error) {
	for {
		//nolint:gosec // Only fixed event-type SQL is concatenated; values use placeholders.
		rows, err := tx.QueryContext(ctx, `SELECT e.id FROM events e WHERE e.project_id=? AND e.id>? AND `+federationPushEventTypeCondition("e.type")+` ORDER BY e.id LIMIT 100`, projectID, after)
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
			event, err := scanEvent(tx.QueryRowContext(ctx, eventSelectByID, id))
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
	byUID := func(ref string) (db.Issue, error) {
		issue, err := eventIssueByUID(ctx, tx, ref)
		if !errors.Is(err, db.ErrNotFound) {
			return issue, err
		}
		// Purged endpoints may still have a unique retained creation owner.
		var count int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT project_id),COALESCE(MIN(project_id),0) FROM events WHERE issue_uid=? AND type IN ('issue.created','issue.snapshot')`, ref).Scan(&count, &issue.ProjectID)
		if err != nil {
			return issue, err
		}
		if count != 1 {
			return issue, db.ErrNotFound
		}
		return issue, nil
	}
	lookup := db.NewEventIssueLookup(byUID, func(project, ref string) (db.Issue, error) { return eventIssueByRef(ctx, tx, project, ref) })
	return db.RelayEventReferencedProjects(event, refs, lookup,
		func(ref string) (db.Issue, error) {
			var issueUID string
			err := tx.QueryRowContext(ctx, `SELECT i.uid FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=?`, ref).Scan(&issueUID)
			if errors.Is(err, sql.ErrNoRows) {
				return db.Issue{}, db.ErrNotFound
			}
			if err != nil {
				return db.Issue{}, err
			}
			return byUID(issueUID)
		},
		func(ref string) (db.Issue, error) {
			var issueUID string
			var count int
			err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(uid),'') FROM issues WHERE short_id=?`, ref).Scan(&count, &issueUID)
			if err != nil {
				return db.Issue{}, err
			}
			if count != 1 {
				return db.Issue{}, db.ErrNotFound
			}
			return byUID(issueUID)
		})
}

func (d *Store) authorizeRelayEventTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment, event db.RemoteEvent) error {
	projects, err := relayEventProjectsTx(ctx, tx, event)
	if err != nil {
		return err
	}
	for _, projectID := range projects {
		var projectUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=? AND deleted_at IS NULL`, projectID).Scan(&projectUID); err != nil {
			return db.RelayProjectLookupError(err)
		}
		if err := d.ProjectAccessTransactionFence(grant.Actor, []string{projectUID})(ctx, tx); err != nil {
			return err
		}
		if err := db.CheckProjectAccessTransaction(ctx, tx, projectUID); err != nil {
			return err
		}
	}
	return nil
}
