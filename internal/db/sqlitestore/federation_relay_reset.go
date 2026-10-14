package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"go.kenn.io/kata/internal/db"
)

// InstallRelayReset installs a forwarded root checkpoint under the relay
// transaction; db.RelayResetProtocol decides what it replaces and keeps.
func (d *Store) InstallRelayReset(ctx context.Context, bindingUID string, manifest db.RootResetManifest, snapshot db.RootResetSnapshot, translation db.RelayResetTranslation) error {
	return d.relayTx(ctx, func(tx *sql.Tx) error {
		return d.relayReset(tx).InstallReset(ctx, bindingUID, manifest, snapshot, translation)
	})
}

// relayResetBackend gives the shared relay reset protocol this store's SQL
// inside one transaction.
type relayResetBackend struct {
	d  *Store
	tx *sql.Tx
}

func (d *Store) relayReset(tx *sql.Tx) db.RelayResetProtocol {
	return db.RelayResetProtocol{Backend: relayResetBackend{d: d, tx: tx}, InstanceUID: d.instanceUID}
}

func (b relayResetBackend) RelayQuarantineOpen(ctx context.Context, projectID int64) (bool, error) {
	var open bool
	err := b.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_quarantine WHERE project_id=? AND skipped_at IS NULL)`, projectID).Scan(&open)
	return open, err
}

func (b relayResetBackend) ExternalRootBindingExists(ctx context.Context, projectID int64) (bool, error) {
	var exists bool
	err := b.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM external_root_bindings WHERE project_id=?)`, projectID).Scan(&exists)
	return exists, err
}

func (b relayResetBackend) ValidateRelayResetEvents(projectUID string, events []db.RemoteEvent) error {
	known := map[string]struct{}{}
	for _, event := range events {
		rememberIngestIssueUIDs(event, known)
	}
	for _, event := range events {
		if err := validateFederationProjectEvent(projectUID, event.OriginInstanceUID, event, known, true); err != nil {
			return err
		}
	}
	return nil
}

func (b relayResetBackend) ReplaceRelayHistory(ctx context.Context, projectID int64, projectUID string) error {
	//nolint:gosec // Only fixed event-type SQL is concatenated; values use placeholders.
	if _, err := b.tx.ExecContext(ctx, `DELETE FROM events WHERE project_id=? AND NOT (type IN (`+db.RelayResetRetainedEventTypesSQL()+`) AND issue_uid IS NULL AND issue_id IS NULL AND NOT EXISTS(SELECT 1 FROM federation_event_provenance p WHERE p.project_uid=? AND p.event_uid=events.uid))`, projectID, projectUID); err != nil {
		return err
	}
	return clearFederatedProjection(ctx, b.tx, projectID)
}

func (b relayResetBackend) DeleteProjectProvenance(ctx context.Context, projectUID string) error {
	for _, table := range []string{"federation_entity_provenance", "federation_event_provenance"} {
		//nolint:gosec // Table names come from the fixed local list; project ID is bound.
		if _, err := b.tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE project_uid=?`, projectUID); err != nil {
			return err
		}
	}
	return nil
}

func (b relayResetBackend) InsertRetiredRootKey(ctx context.Context, key db.RootKeyPin) error {
	_, err := b.tx.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key,active) VALUES(?,?,?,?,0)`, key.ProjectUID, key.AuthorityUID, key.KeyID, base64.StdEncoding.EncodeToString(key.PublicKey))
	return err
}

func (b relayResetBackend) InsertRelayResetEvent(ctx context.Context, projectID int64, event db.RemoteEvent) error {
	_, err := b.tx.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,related_issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.EventUID, event.OriginInstanceUID, projectID, event.ProjectName, event.IssueUID, event.RelatedIssueUID, event.Type, event.Actor, string(event.Payload), event.HLCPhysicalMS, event.HLCCounter, event.ContentHash, event.CreatedAt.UTC().Format(db.EventTimestampFormat))
	return err
}

func (b relayResetBackend) MaterializeFederatedProject(ctx context.Context, projectID int64) error {
	return b.d.materializeFederatedProjectTx(ctx, b.tx, projectID, true, nil, nil)
}

func (b relayResetBackend) DeleteOrphanRelayArtifacts(ctx context.Context, projectUID string) error {
	_, err := b.tx.ExecContext(ctx, `DELETE FROM federation_embedding_artifacts WHERE project_uid=? AND NOT EXISTS(SELECT 1 FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.uid=federation_embedding_artifacts.project_uid AND i.uid=federation_embedding_artifacts.issue_uid)`, projectUID)
	return err
}

func (b relayResetBackend) DeleteOrphanPendingCreations(ctx context.Context, projectID int64, projectUID string) error {
	return deleteOrphanPendingCreationsTx(ctx, b.tx, projectID, projectUID)
}

func (b relayResetBackend) RetainedRelayArtifact(ctx context.Context, projectUID, digest string, now time.Time) (db.RelayRetainedArtifactRow, bool, error) {
	var row db.RelayRetainedArtifactRow
	err := b.tx.QueryRowContext(ctx, `SELECT a.artifact,i.title,i.body FROM federation_embedding_artifacts a JOIN issues i ON i.uid=a.issue_uid JOIN projects p ON p.id=i.project_id AND p.uid=a.project_uid WHERE a.project_uid=? AND a.digest=? AND (a.staging_expires_at IS NULL OR a.staging_expires_at>?)`, projectUID, digest, now.UTC().Format(time.RFC3339Nano)).Scan(&row.Artifact, &row.Title, &row.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return db.RelayRetainedArtifactRow{}, false, nil
	}
	if err != nil {
		return db.RelayRetainedArtifactRow{}, false, err
	}
	return row, true, nil
}

func (b relayResetBackend) ActivateStagedRelayArtifact(ctx context.Context, projectUID, digest string) error {
	_, err := b.tx.ExecContext(ctx, `UPDATE federation_embedding_artifacts SET staging_expires_at=NULL WHERE project_uid=? AND digest=?`, projectUID, digest)
	return err
}

func (b relayResetBackend) InsertEventProvenance(ctx context.Context, projectUID string, receipt db.AttributionReceipt, raw string) error {
	_, err := b.tx.ExecContext(ctx, `INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES(?,?,?,?,?,?,?)`, projectUID, receipt.EventUID, receipt.ContentHash, receipt.ResetEpoch, receipt.Sequence, receipt.KeyID, raw)
	return err
}

func (b relayResetBackend) InsertEntityProvenance(ctx context.Context, projectUID string, ref db.EntityProvenance) error {
	_, err := b.tx.ExecContext(ctx, `INSERT INTO federation_entity_provenance(project_uid,kind,entity_uid,event_uid) VALUES(?,?,?,?)`, projectUID, ref.Kind, ref.EntityUID, ref.EventUID)
	return err
}

func (b relayResetBackend) DeletePendingCreation(ctx context.Context, projectUID, kind, entityUID string) error {
	return deletePendingCreationTx(ctx, b.tx, projectUID, kind, entityUID)
}

func (b relayResetBackend) UpdateRelayBindingConfig(ctx context.Context, projectID int64, raw string) error {
	_, err := b.tx.ExecContext(ctx, `UPDATE federation_bindings SET relay_config=? WHERE project_id=?`, raw, projectID)
	return err
}

func (b relayResetBackend) InsertRelayCursor(ctx context.Context, seed db.RelayCursorSeed) error {
	_, err := b.tx.ExecContext(ctx, `INSERT INTO federation_relay_cursors(project_uid,binding_uid,stream,reset_epoch,offered_through,accepted_through) VALUES(?,?,?,?,?,?)`, seed.ProjectUID, seed.BindingUID, seed.Stream, seed.Epoch, seed.Baseline, seed.Baseline)
	return err
}

func (b relayResetBackend) ReserveAttributionUIReset(ctx context.Context, projectUID string) error {
	return reserveAttributionUIResetTx(ctx, b.tx, projectUID)
}
