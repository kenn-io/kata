package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

func (b relayResetBackend) RelayServingBinding(ctx context.Context, projectID int64) (db.FederationBinding, error) {
	binding, _, err := b.d.relayServingBindingTx(ctx, b.tx, projectID, db.RelayProtocolVersion)
	return binding, err
}

func (b relayResetBackend) UpstreamRelayGrant(ctx context.Context, bindingUID string) (db.FederationEnrollment, error) {
	return b.d.upstreamRelayGrantTx(ctx, b.tx, bindingUID)
}

func (b relayResetBackend) RelayGrant(ctx context.Context, bindingUID, capability string) (db.FederationEnrollment, error) {
	return b.d.relayGrantTx(ctx, b.tx, bindingUID, capability)
}

func (b relayResetBackend) SeedRelayArtifactManifests(ctx context.Context, grant db.FederationEnrollment, projectUID string) error {
	return b.d.seedRelayArtifactManifestsTx(ctx, b.tx, grant, projectUID)
}

func (b relayResetBackend) RelayAcceptedInboxPage(ctx context.Context, page db.RelayRetainedPage) ([]db.RelayRetainedEnvelopeRow, error) {
	rows, err := b.tx.QueryContext(ctx, `SELECT sequence,source_uid,source_hash,envelope FROM federation_relay_inbox WHERE project_uid=$1 AND binding_uid=$2 AND stream=$3 AND reset_epoch=$4 AND sequence>$5 AND accepted=1 ORDER BY sequence LIMIT $6`, page.ProjectUID, page.BindingUID, page.Stream, page.Epoch, page.After, page.Limit)
	if err != nil {
		return nil, err
	}
	return scanRetainedEnvelopeRows(rows)
}

func (b relayResetBackend) RelayOutboxPage(ctx context.Context, page db.RelayRetainedPage) ([]db.RelayRetainedEnvelopeRow, error) {
	rows, err := b.tx.QueryContext(ctx, `SELECT id,source_uid,source_hash,envelope FROM federation_relay_outbox WHERE project_uid=$1 AND binding_uid=$2 AND stream=$3 AND reset_epoch=$4 AND id>$5 ORDER BY id LIMIT $6`, page.ProjectUID, page.BindingUID, page.Stream, page.Epoch, page.After, page.Limit)
	if err != nil {
		return nil, err
	}
	return scanRetainedEnvelopeRows(rows)
}

func scanRetainedEnvelopeRows(rows *sql.Rows) ([]db.RelayRetainedEnvelopeRow, error) {
	defer func() { _ = rows.Close() }()
	var page []db.RelayRetainedEnvelopeRow
	for rows.Next() {
		var row db.RelayRetainedEnvelopeRow
		if err := rows.Scan(&row.Sequence, &row.SourceUID, &row.SourceHash, &row.Envelope); err != nil {
			return nil, err
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return page, rows.Close()
}
