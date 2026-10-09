package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"iter"

	"go.kenn.io/kata/internal/db"
)

// ExportRelayState is restricted to full owner backups by its JSONL callers.
func (d *Store) ExportRelayState(ctx context.Context) iter.Seq2[db.ImportRecord, error] {
	return func(yield func(db.ImportRecord, error) bool) {
		{
			rows, err := d.exportQueryContext(ctx, `SELECT id,project_uid,binding_uid,stream,reset_epoch,source_uid,source_hash,envelope,emitted,acknowledged FROM federation_relay_outbox ORDER BY binding_uid,stream,reset_epoch,id`)
			if err != nil {
				yield(nil, err)
				return
			}
			keep := true
			for rows.Next() {
				var r db.RelayOutboxExport
				var emitted, acknowledged int
				err = rows.Scan(&r.ID, &r.ProjectUID, &r.BindingUID, &r.Stream, &r.ResetEpoch, &r.SourceUID, &r.SourceHash, &r.Envelope, &emitted, &acknowledged)
				if err != nil {
					break
				}
				r.Emitted = emitted != 0
				r.Acknowledged = acknowledged != 0
				if err = db.ValidateRelayDeliveryRecord(&r); err != nil {
					break
				}
				if !yield(&r, nil) {
					keep = false
					break
				}
			}
			if err == nil {
				err = rows.Err()
			}
			_ = rows.Close()
			if err != nil {
				yield(nil, fmt.Errorf("export federation_relay_outbox: %w", err))
				return
			}
			if !keep {
				return
			}
		}
		{
			rows, err := d.exportQueryContext(ctx, `SELECT project_uid,binding_uid,stream,reset_epoch,sequence,source_uid,source_hash,envelope_digest,envelope,accepted FROM federation_relay_inbox ORDER BY binding_uid,stream,reset_epoch,sequence`)
			if err != nil {
				yield(nil, err)
				return
			}
			keep := true
			for rows.Next() {
				var r db.RelayInboxExport
				var accepted int
				err = rows.Scan(&r.ProjectUID, &r.BindingUID, &r.Stream, &r.ResetEpoch, &r.Sequence, &r.SourceUID, &r.SourceHash, &r.EnvelopeDigest, &r.Envelope, &accepted)
				if err != nil {
					break
				}
				r.Accepted = accepted != 0
				if err = db.ValidateRelayDeliveryRecord(&r); err != nil {
					break
				}
				if !yield(&r, nil) {
					keep = false
					break
				}
			}
			if err == nil {
				err = rows.Err()
			}
			_ = rows.Close()
			if err != nil {
				yield(nil, fmt.Errorf("export federation_relay_inbox: %w", err))
				return
			}
			if !keep {
				return
			}
		}
		{
			rows, err := d.exportQueryContext(ctx, `SELECT project_uid,binding_uid,stream,reset_epoch,offered_through,accepted_through,emitted_through,acknowledged_through FROM federation_relay_cursors ORDER BY binding_uid,stream,reset_epoch`)
			if err != nil {
				yield(nil, err)
				return
			}
			keep := true
			for rows.Next() {
				var r db.RelayCursorExport
				err = rows.Scan(&r.ProjectUID, &r.BindingUID, &r.Stream, &r.ResetEpoch, &r.OfferedThrough, &r.AcceptedThrough, &r.EmittedThrough, &r.AcknowledgedThrough)
				if err != nil {
					break
				}
				if err = db.ValidateRelayDeliveryRecord(&r); err != nil {
					break
				}
				if !yield(&r, nil) {
					keep = false
					break
				}
			}
			if err == nil {
				err = rows.Err()
			}
			_ = rows.Close()
			if err != nil {
				yield(nil, fmt.Errorf("export federation_relay_cursors: %w", err))
				return
			}
			if !keep {
				return
			}
		}
	}
}
func importRelayState(ctx context.Context, tx *sql.Tx, record db.ImportRecord) error {
	if err := db.ValidateRelayDeliveryRecord(record); err != nil {
		return err
	}
	switch r := record.(type) {
	case *db.RelayOutboxExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_outbox(id,project_uid,binding_uid,stream,reset_epoch,source_uid,source_hash,envelope,emitted,acknowledged) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, r.ID, r.ProjectUID, r.BindingUID, r.Stream, r.ResetEpoch, r.SourceUID, r.SourceHash, r.Envelope, boolAsInt(r.Emitted), boolAsInt(r.Acknowledged))
		return err
	case *db.RelayInboxExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_inbox(project_uid,binding_uid,stream,reset_epoch,sequence,source_uid,source_hash,envelope_digest,envelope,accepted) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, r.ProjectUID, r.BindingUID, r.Stream, r.ResetEpoch, r.Sequence, r.SourceUID, r.SourceHash, r.EnvelopeDigest, r.Envelope, boolAsInt(r.Accepted))
		return err
	case *db.RelayCursorExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_relay_cursors(project_uid,binding_uid,stream,reset_epoch,offered_through,accepted_through,emitted_through,acknowledged_through) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, r.ProjectUID, r.BindingUID, r.Stream, r.ResetEpoch, r.OfferedThrough, r.AcceptedThrough, r.EmittedThrough, r.AcknowledgedThrough)
		return err
	}
	return fmt.Errorf("unsupported relay restore record")
}
func boolAsInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
