package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// RelayMetadataRequiresCrossLinks reads checkpoint content without preparing a reset.
func (d *Store) RelayMetadataRequiresCrossLinks(ctx context.Context, projectID int64) (bool, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, projectID))
	if err != nil {
		return false, err
	}
	pin, err := rootPinTx(ctx, tx, project.UID, "")
	unpinned := errors.Is(err, db.ErrNotFound)
	if err != nil && !unpinned {
		return false, err
	}
	if unpinned {
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, projectID))
		if err != nil {
			return false, err
		}
		pin.AuthorityUID, err = relayProjectRootTx(ctx, tx, project.UID, binding, d.InstanceUID())
		if err != nil {
			return false, err
		}
		if pin.AuthorityUID == "" {
			return false, db.ErrNotFound
		}
	}
	required := func(event db.RemoteEvent) (bool, error) {
		feature, err := db.RelayContentRequiredFeatures(event, pin.AuthorityUID, func(uid string) (string, error) {
			var projectUID string
			var peerID int64
			err := tx.QueryRowContext(ctx, `SELECT p.uid,p.id FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1`, uid).Scan(&projectUID, &peerID)
			if errors.Is(err, sql.ErrNoRows) {
				return "", nil
			}
			if err != nil || !unpinned || peerID == projectID {
				return projectUID, err
			}
			shared, err := relaySharedProjectsTx(ctx, tx, projectID, peerID, false)
			if err != nil {
				return "", err
			}
			if !shared {
				return project.UID, nil
			}
			return projectUID, nil
		})
		return feature != "", err
	}
	for after := int64(0); ; {
		events, err := relayCheckpointEventPageTx(ctx, tx, projectID, after)
		if err != nil {
			return false, err
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			remote := db.RemoteEventFromStored(event)
			crosses, err := relayEventCrossesProjectTx(ctx, tx, projectID, remote)
			if err != nil {
				return false, err
			}
			if !crosses || (unpinned && remote.OriginInstanceUID == pin.AuthorityUID) {
				needed, err := required(remote)
				if needed || err != nil {
					return needed, err
				}
			}
		}
		after = events[len(events)-1].ID
	}
	issues, err := d.relayCheckpointIssuesTx(ctx, tx, projectID)
	if err != nil {
		return false, err
	}
	for _, issue := range issues {
		event := db.RemoteEvent{ProjectUID: project.UID, OriginInstanceUID: pin.AuthorityUID, IssueUID: &issue.IssueUID, Type: "issue.snapshot", Payload: jsontext.Value(issue.Payload)}
		needed, err := required(event)
		if needed || err != nil {
			return needed, err
		}
	}
	return false, nil
}
