package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// The caller first verifies a root receipt and current project authority. A
// compacted source may still be retained as original immutable outbox bytes;
// only an exact hash match can recover its creation reference. A snapshot or
// author label cannot supply this evidence.
func attributionSourceTx(ctx context.Context, tx db.Transaction, projectID int64, receipt db.AttributionReceipt) (db.RemoteEvent, error) {
	var source db.RemoteEvent
	var issue *string
	var payload string
	err := tx.QueryRowContext(ctx, `SELECT uid,type,actor,issue_uid,payload,content_hash FROM events WHERE project_id=$1 AND uid=$2`, projectID, receipt.EventUID).Scan(&source.EventUID, &source.Type, &source.Actor, &issue, &payload, &source.ContentHash)
	if err == nil {
		source.ProjectUID, source.IssueUID, source.Payload = receipt.ProjectUID, issue, []byte(payload)
		return source, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return source, err
	}
	var retained string
	if err = tx.QueryRowContext(ctx, `SELECT envelope FROM federation_relay_outbox WHERE project_uid=$1 AND stream=$2 AND source_uid=$3 AND source_hash=$4 ORDER BY id LIMIT 1`, receipt.ProjectUID, db.RelayStreamEvent, receipt.EventUID, receipt.ContentHash).Scan(&retained); err != nil {
		return source, err
	}
	var envelope db.RelayEnvelope
	if err := json.Unmarshal([]byte(retained), &envelope, json.RejectUnknownMembers(true)); err != nil {
		return source, err
	}
	if envelope.Stream != db.RelayStreamEvent || envelope.ProjectUID != receipt.ProjectUID || envelope.SourceUID != receipt.EventUID || envelope.SourceHash != receipt.ContentHash {
		return source, db.ErrRemoteEventHashMismatch
	}
	source, err = db.DecodeRelaySourceEvent(envelope.Body)
	if err != nil {
		return source, err
	}
	if source.ProjectUID != receipt.ProjectUID || source.EventUID != receipt.EventUID || source.ContentHash != receipt.ContentHash {
		return source, db.ErrRemoteEventHashMismatch
	}
	return source, nil
}
