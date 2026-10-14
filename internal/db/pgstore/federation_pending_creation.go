package pgstore

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
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, event.ProjectID))
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, db.PendingCreationMetadataPrefix+ref.ProjectUID+"."+ref.Kind+"."+ref.EntityUID, string(value)); err != nil {
			return err
		}
	}
	return nil
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// A retained root receipt is the proof the pending row stood in for.
func deletePendingCreationTx(ctx context.Context, execer contextExecer, projectUID, kind, entityUID string) error {
	_, err := execer.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, db.PendingCreationMetadataPrefix+projectUID+"."+kind+"."+entityUID)
	return mapSQLError(err, nil)
}

// A rebuilt projection keeps no creation intent for entities it no longer
// holds, such as rows restored from an older backup for issues the root has
// since purged. Rows for entities still present stay until their receipt.
func deleteOrphanPendingCreationsTx(ctx context.Context, execer contextExecer, projectID int64, projectUID string) error {
	issuePrefix := db.PendingCreationMetadataPrefix + projectUID + ".issue."
	if _, err := execer.ExecContext(ctx, `DELETE FROM meta WHERE left(key,length($1::text))=$1::text AND NOT EXISTS(SELECT 1 FROM issues i WHERE i.project_id=$2 AND $1::text || i.uid=meta.key)`, issuePrefix, projectID); err != nil {
		return mapSQLError(err, nil)
	}
	commentPrefix := db.PendingCreationMetadataPrefix + projectUID + ".comment."
	_, err := execer.ExecContext(ctx, `DELETE FROM meta WHERE left(key,length($1::text))=$1::text AND NOT EXISTS(SELECT 1 FROM comments c JOIN issues i ON i.id=c.issue_id WHERE i.project_id=$2 AND $1::text || c.uid=meta.key)`, commentPrefix, projectID)
	return mapSQLError(err, nil)
}

// Purged entities keep no creation intent. Comments go with their issue.
func deleteIssuePendingCreationTx(ctx context.Context, execer contextExecer, issue db.Issue) error {
	_, err := execer.ExecContext(ctx, `DELETE FROM meta WHERE key=$1 OR key IN (SELECT $2::text || uid FROM comments WHERE issue_id=$3)`,
		db.PendingCreationMetadataPrefix+issue.ProjectUID+".issue."+issue.UID,
		db.PendingCreationMetadataPrefix+issue.ProjectUID+".comment.", issue.ID)
	return mapSQLError(err, nil)
}
