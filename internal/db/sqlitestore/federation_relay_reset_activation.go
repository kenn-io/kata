package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) activateRelayCheckpointTx(ctx context.Context, tx *sql.Tx, grant db.FederationEnrollment) (db.FederationEnrollment, error) {
	return d.relayReset(tx).ActivateCheckpoint(ctx, grant)
}

func (b relayResetBackend) Meta(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := b.tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (b relayResetBackend) PutMeta(ctx context.Context, key, value string) error {
	_, err := b.tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (b relayResetBackend) DeleteMeta(ctx context.Context, key string) error {
	_, err := b.tx.ExecContext(ctx, `DELETE FROM meta WHERE key=?`, key)
	return err
}

func (b relayResetBackend) RootPin(ctx context.Context, projectUID, keyID string) (db.RootKeyPin, error) {
	return rootPinTx(ctx, b.tx, projectUID, keyID)
}

func (b relayResetBackend) RelayUnacknowledgedEnvelopes(ctx context.Context, bindingUID string, epoch int64, keepStream string, boundaryID int64) ([]string, error) {
	rows, err := b.tx.QueryContext(ctx, `SELECT envelope FROM federation_relay_outbox WHERE binding_uid=? AND reset_epoch=? AND acknowledged=0 AND (stream=? OR id>?) ORDER BY id`, bindingUID, epoch, keepStream, boundaryID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var envelopes []string
	for rows.Next() {
		var envelope string
		if err := rows.Scan(&envelope); err != nil {
			return nil, err
		}
		envelopes = append(envelopes, envelope)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return envelopes, rows.Close()
}

func (b relayResetBackend) SetRelayGrantEpoch(ctx context.Context, grantID, epoch int64) error {
	_, err := b.tx.ExecContext(ctx, `UPDATE federation_enrollments SET relay_reset_epoch=? WHERE id=?`, epoch, grantID)
	return err
}

func (b relayResetBackend) QueueRelayBody(ctx context.Context, body db.RelayQueuedBody) error {
	return queueRelayBodyTx(ctx, b.tx, body.ProjectID, body.ProjectUID, body.Stream, body.SourceUID, body.SourceHash, body.Body, b.d.instanceUID, body.Grant)
}
