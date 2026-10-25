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
	authority, err := relayRootAuthorityTx(ctx, tx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	peer, err := relayRootAuthorityTx(ctx, tx, peerID)
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
func relayRootAuthorityTx(ctx context.Context, tx *sql.Tx, projectID int64) (string, error) {
	var projectUID, role, instance string
	err := tx.QueryRowContext(ctx, `SELECT p.uid,b.role,m.value FROM projects p JOIN federation_bindings b ON b.project_id=p.id JOIN meta m ON m.key='instance_uid' WHERE p.id=? AND p.deleted_at IS NULL AND b.enabled=1`, projectID).Scan(&projectUID, &role, &instance)
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
		// Only relationship mutations require root authorship. Ordinary close
		// freezes references whose projects are still authorized at release.
		if db.FederationEventAffectsLinks(event.Type) {
			authority, err := relayRootAuthorityTx(ctx, tx, projectID)
			if errors.Is(err, db.ErrNotFound) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if event.OriginInstanceUID != authority {
				return true, nil
			}
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
	return db.RelayEventReferencedProjectsTx(ctx, tx, event, refs,
		func(uid string) (db.Issue, error) { return eventIssueByUID(ctx, tx, uid) },
		func(int) string { return "?" }, func(field string) string { return "json_extract(e.payload,'$." + field + "')" })
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
