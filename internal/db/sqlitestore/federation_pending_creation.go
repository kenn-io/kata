package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// Only new local creation under a negotiated upstream earns pending status.
// Historical snapshots and replay cannot promote source labels into proof.
func (d *Store) retainPendingCreationTx(ctx context.Context, tx *sql.Tx, event db.Event) error {
	if db.RelayStateCapture(ctx) || event.OriginInstanceUID != d.instanceUID {
		return nil
	}
	_, refs, err := db.EventCreationProvenance(db.RemoteEventFromStored(event))
	if err != nil || len(refs) == 0 {
		return err
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=?`, event.ProjectID))
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if binding.Role != db.FederationRoleSpoke || binding.RelayConfig == nil {
		return nil
	}
	for _, ref := range refs {
		value, err := json.Marshal(struct {
			EventUID    string `json:"event_uid"`
			ContentHash string `json:"content_hash"`
		}{event.UID, event.ContentHash})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, db.PendingCreationMetadataPrefix+ref.ProjectUID+"."+ref.Kind+"."+ref.EntityUID, string(value)); err != nil {
			return err
		}
	}
	return nil
}
