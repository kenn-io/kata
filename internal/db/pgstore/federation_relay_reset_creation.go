package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

// CreateRelayReset builds or forwards a signed checkpoint for one downstream
// enrollment under the relay transaction; db.RelayResetProtocol decides what
// the checkpoint carries and when a retained one is disclosed again.
func (d *Store) CreateRelayReset(ctx context.Context, bindingUID string, signer db.RootAttributionSigner) (db.RelayResetCheckpoint, error) {
	var result db.RelayResetCheckpoint
	err := d.relayTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = d.relayReset(tx).CreateReset(ctx, bindingUID, signer)
		return err
	})
	if err != nil {
		return db.RelayResetCheckpoint{}, err
	}
	return result, nil
}

func (b relayResetBackend) Project(ctx context.Context, projectID int64) (db.Project, error) {
	return scanProject(b.tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, projectID))
}

func (b relayResetBackend) RelayCheckpointNeedsRefresh(ctx context.Context, projectUID string, checkpoint db.RelayResetCheckpoint) (bool, error) {
	return b.d.relayCheckpointNeedsRefreshTx(ctx, b.tx, projectUID, checkpoint)
}

func (b relayResetBackend) LatestEventID(ctx context.Context, projectID int64) (int64, error) {
	var latest int64
	err := b.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE project_id=$1`, projectID).Scan(&latest)
	return latest, err
}

func (b relayResetBackend) InsertFederationBaselineEvents(ctx context.Context, project db.Project, actor string) (db.Event, error) {
	return b.d.insertFederationBaselineEventsTx(ctx, b.tx, project, actor)
}

func (b relayResetBackend) InsertRelayCheckpointStateEvent(ctx context.Context, state db.RelayCheckpointStateEvent) error {
	_, err := b.d.insertEventTx(ctx, b.tx, eventInsert{ProjectID: state.Project.ID, ProjectUID: state.Project.UID, ProjectName: state.Project.Name, Type: state.Type, Actor: state.Actor, Payload: state.Payload, HLC: &state.Clock, CreatedAt: state.CreatedAt.UTC().Format(db.EventTimestampFormat)})
	return err
}

func (b relayResetBackend) RelayCheckpointRecords(ctx context.Context, project db.Project, boundaryID int64) (db.RelayCheckpointRecords, error) {
	return b.d.relayCheckpointRecordsTx(ctx, b.tx, project, boundaryID)
}

func (b relayResetBackend) RelayCheckpointCapacity() int64 {
	return b.d.relayCheckpointCapacity()
}

func (b relayResetBackend) MetaValuesWithPrefix(ctx context.Context, prefix string) ([]string, error) {
	rows, err := b.tx.QueryContext(ctx, `SELECT value FROM meta WHERE substr(key,1,length($1))=$2`, prefix, prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, rows.Close()
}

func (b relayResetBackend) RelayOutboxMaxID(ctx context.Context, projectUID, stream string) (int64, error) {
	var latest int64
	err := b.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM federation_relay_outbox WHERE project_uid=$1 AND stream=$2`, projectUID, stream).Scan(&latest)
	return latest, err
}
