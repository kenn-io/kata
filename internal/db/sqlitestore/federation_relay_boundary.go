package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
	relayed, err := relayProjectsActiveTx(ctx, tx, fromProject, toProject)
	if err != nil {
		return err
	}
	if relayed {
		shared, err := relaySharedProjectsTx(ctx, tx, fromProject, toProject, true)
		if err != nil {
			return err
		}
		if shared {
			return nil
		}
		return errors.Join(db.ErrFederatedReadOnly, db.ErrFederationCrossProjectBoundary)
	}
	return nil
}

func ensureRelayLinkRemovalBoundaryTx(ctx context.Context, tx *sql.Tx, link db.Link, eventIssue db.Issue) error {
	err := ensureRelayLinkBoundaryTx(ctx, tx, link.FromIssueID, link.ToIssueID)
	if !errors.Is(err, db.ErrFederationCrossProjectBoundary) || (eventIssue.ID != link.FromIssueID && eventIssue.ID != link.ToIssueID) {
		return err
	}
	root, rootErr := relaySharedProjectsTx(ctx, tx, eventIssue.ProjectID, eventIssue.ProjectID, true)
	if rootErr != nil {
		return rootErr
	}
	if root {
		return nil
	}
	return err
}

func relayProjectsActiveTx(ctx context.Context, tx *sql.Tx, fromProject, toProject int64) (bool, error) {
	var relayed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_bindings b WHERE b.project_id IN(?,?) AND (b.relay_config IS NOT NULL OR EXISTS(SELECT 1 FROM federation_enrollments e WHERE e.project_id=b.project_id AND e.relay_protocol_version=1 AND e.revoked_at IS NULL)))`, fromProject, toProject).Scan(&relayed)
	return relayed, err
}

func validateLegacyRelayAdmissionTx(ctx context.Context, tx *sql.Tx, projectID int64, event db.RemoteEvent) error {
	crossesProject := func(ctx context.Context, peerProject int64) (bool, error) {
		if peerProject == projectID {
			return false, nil
		}
		return relayProjectsActiveTx(ctx, tx, projectID, peerProject)
	}
	return db.ValidateLegacyRelayAdmission(ctx, event, func(ctx context.Context, peer string) (bool, error) {
		var peerProject int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT project_id FROM issues WHERE uid=?),?)`, peer, projectID).Scan(&peerProject); err != nil {
			return false, err
		}
		return crossesProject(ctx, peerProject)
	}, func(ctx context.Context, shortID string) (bool, error) {
		// Legacy parents resolve only when the short ID is unique across projects.
		var matches int
		var peerProject int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(project_id),0) FROM issues WHERE short_id=?`, shortID).Scan(&matches, &peerProject); err != nil {
			return false, err
		}
		if matches != 1 {
			return false, nil
		}
		return crossesProject(ctx, peerProject)
	}, func(ctx context.Context, ref string) (bool, error) {
		// Membership, rather than caller visibility, decides admission.
		target, err := eventIssueByRef(db.WithAuthorizedProjects(ctx, nil), tx, event.ProjectUID, ref)
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return crossesProject(ctx, target.ProjectID)
	})
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

// relaySharedProjectsTx checks the binding group and its root authority.
func relaySharedProjectsTx(ctx context.Context, tx *sql.Tx, projectID, peerID int64, localWrite bool) (bool, error) {
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
	peer, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=?`, peerID))
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !peer.Enabled || (localWrite && peer.Role != db.FederationRoleHub) {
		return false, nil
	}
	group, err := federationBindingGroupProjectIDs(ctx, tx, binding)
	if err != nil {
		return false, err
	}
	if !slices.Contains(group, peerID) {
		return false, nil
	}
	var instance string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&instance); err != nil {
		return false, err
	}
	root := func(id int64, binding db.FederationBinding) (string, error) {
		var uid string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=? AND deleted_at IS NULL`, id).Scan(&uid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", nil
			}
			return "", err
		}
		return relayProjectRootTx(ctx, tx, uid, binding, instance)
	}
	first, err := root(projectID, binding)
	if err != nil {
		return false, err
	}
	second, err := root(peerID, peer)
	if err != nil {
		return false, err
	}
	return first != "" && first == second && (!localWrite || first == instance), nil
}

func relayProjectRootTx(ctx context.Context, tx *sql.Tx, projectUID string, binding db.FederationBinding, instance string) (string, error) {
	pin, err := rootPinTx(ctx, tx, projectUID, "")
	if errors.Is(err, db.ErrNotFound) {
		if binding.Enabled && binding.Role == db.FederationRoleHub {
			return instance, nil
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return pin.AuthorityUID, nil
}

// Enrollment retains links only when their projects share a root and group.
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
		shared, err := relaySharedProjectsTx(ctx, tx, projectID, peer, false)
		if err != nil {
			return err
		}
		if !shared {
			return errors.Join(db.ErrFederatedReadOnly, db.ErrFederationCrossProjectBoundary)
		}
	}
	return nil
}

// Original cross-project event bytes remain on the owner store. Initial relay
// sync uses a signed current-state checkpoint rather than rewriting or sharing
// those historical private endpoints.
func relayEventCrossesProjectTx(ctx context.Context, tx *sql.Tx, projectID int64, event db.RemoteEvent) (bool, error) {
	payload := db.PayloadMap(event.Payload)
	if event.Type == "issue.closed" {
		crossing, err := relayCloseParentCrossesProjectTx(ctx, tx, projectID, payload)
		if err != nil || crossing {
			return crossing, err
		}
		crossing, err = relayCloseEvidenceCrossesProjectTx(ctx, tx, projectID, payload)
		if err != nil || crossing {
			return crossing, err
		}
	}
	refs, err := payloadReferencedIssueUIDs(event, payload)
	if err != nil {
		return false, err
	}
	if event.IssueUID != nil {
		refs = append(refs, *event.IssueUID)
	}
	rootLink := false
	legacyCleanup := false
	if db.FederationEventAffectsLinks(event.Type) {
		pin, pinErr := rootPinTx(ctx, tx, event.ProjectUID, "")
		if pinErr != nil && !errors.Is(pinErr, db.ErrNotFound) {
			return false, pinErr
		}
		rootLink = pinErr == nil && pin.AuthorityUID == event.OriginInstanceUID
		if !rootLink {
			var receipt bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_event_provenance WHERE project_uid=? AND event_uid=?)`, event.ProjectUID, event.EventUID).Scan(&receipt); err != nil {
				return false, err
			}
			if !receipt {
				crosses := func(ctx context.Context, peer string) (bool, error) {
					var own bool
					err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE uid=? AND project_id=?)`, peer, projectID).Scan(&own)
					return !own, err
				}
				err := db.ValidateLegacyRelayAdmission(ctx, event, crosses, func(context.Context, string) (bool, error) { return false, nil }, func(context.Context, string) (bool, error) { return false, nil })
				if err != nil && !errors.Is(err, db.ErrFederationCrossProjectBoundary) {
					return false, err
				}
				legacyCleanup = err == nil
			}
		}
	}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if (rootLink || legacyCleanup) && (event.IssueUID == nil || ref != *event.IssueUID) {
			var peerID int64
			err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=?`, ref).Scan(&peerID)
			if errors.Is(err, sql.ErrNoRows) && rootLink {
				continue
			}
			if errors.Is(err, sql.ErrNoRows) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if peerID != projectID {
				shared, err := relaySharedProjectsTx(ctx, tx, projectID, peerID, false)
				if err != nil {
					return false, err
				}
				if shared {
					continue
				}
			}
		}
		var own bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE uid=? AND project_id=?)`, ref, projectID).Scan(&own); err != nil {
			return false, err
		}
		if !own {
			return true, nil
		}
	}
	return false, nil
}

func relayCloseEvidenceCrossesProjectTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	payload map[string]jsontext.Value,
) (bool, error) {
	raw, ok := payload["evidence"]
	if !ok {
		return false, nil
	}
	var evidence []db.Evidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return true, nil
	}
	if evidence == nil {
		return true, nil
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=?`, projectID).Scan(&projectUID); err != nil {
		return false, err
	}
	for _, item := range evidence {
		if item.Type != "duplicate-of" && item.Type != "superseded-by" {
			continue
		}
		if item.IssueRef == "" {
			return true, nil
		}
		target, err := eventIssueByRef(ctx, tx, projectUID, item.IssueRef)
		if errors.Is(err, db.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if target.ProjectID != projectID {
			return true, nil
		}
	}
	return false, nil
}

func relayCloseParentCrossesProjectTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	payload map[string]jsontext.Value,
) (bool, error) {
	parentUID, uidPresent := db.StringValue(payload["parent_uid"])
	parentShortID, shortIDPresent := db.StringValue(payload["parent_short_id"])
	if _, exists := payload["parent_uid"]; exists && !uidPresent {
		return true, nil
	}
	if _, exists := payload["parent_short_id"]; exists && !shortIDPresent {
		return true, nil
	}
	if parentUID == "" && parentShortID == "" {
		return false, nil
	}
	var own bool
	if parentUID != "" {
		query := `SELECT EXISTS(SELECT 1 FROM issues WHERE uid=? AND project_id=?`
		args := []any{parentUID, projectID}
		if parentShortID != "" {
			query += ` AND short_id=?`
			args = append(args, parentShortID)
		}
		query += `)`
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&own); err != nil {
			return false, err
		}
		return !own, nil
	}
	var matches int
	var resolvedProjectID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(project_id),0) FROM issues WHERE short_id=?`, parentShortID,
	).Scan(&matches, &resolvedProjectID); err != nil {
		return false, err
	}
	return matches != 1 || resolvedProjectID != projectID, nil
}

func relayHistoryHasBoundaryTx(ctx context.Context, tx *sql.Tx, projectID int64) (bool, error) {
	return relayHistoryHasBoundaryAfterTx(ctx, tx, projectID, 0)
}

// relayHistoryHasBoundaryAfterTx detects cross-project references or a mutation
// retained before its issue's introduction.
func relayHistoryHasBoundaryAfterTx(ctx context.Context, tx *sql.Tx, projectID, after int64) (bool, error) {
	var outOfOrder bool
	//nolint:gosec // Only fixed event-type SQL is concatenated; values use placeholders.
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events m
		WHERE m.project_id=? AND m.id>? AND m.issue_uid IS NOT NULL
		AND m.type LIKE 'issue.%' AND m.type NOT IN ('issue.created','issue.snapshot')
		AND `+federationPushEventTypeCondition("m.type")+`
		AND NOT EXISTS(SELECT 1 FROM events i WHERE i.project_id=m.project_id
		AND i.issue_uid=m.issue_uid AND i.type IN ('issue.created','issue.snapshot') AND i.id<m.id))`, projectID, after).Scan(&outOfOrder); err != nil {
		return false, err
	}
	if outOfOrder {
		return true, nil
	}
	return relayHistoryCrossesProjectAfterTx(ctx, tx, projectID, after)
}

func relayHistoryCrossesProjectAfterTx(ctx context.Context, tx *sql.Tx, projectID, after int64) (bool, error) {
	for {
		events, err := relayCheckpointEventPageTx(ctx, tx, projectID, after)
		if err != nil {
			return false, err
		}
		if len(events) == 0 {
			return false, nil
		}
		for _, event := range events {
			crossing, err := relayEventCrossesProjectTx(ctx, tx, projectID, db.RemoteEventFromStored(event))
			if err != nil || crossing {
				return crossing, err
			}
		}
		after = events[len(events)-1].ID
	}
}
