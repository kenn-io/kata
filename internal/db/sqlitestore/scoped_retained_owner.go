package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"iter"
)

func scopedRetainedOwningReader(transaction db.Transaction) (readQuerier, error) {
	switch handle := transaction.(type) {
	case *sql.Tx:
		if handle != nil {
			return handle, nil
		}
	case *sql.Conn:
		if handle != nil {
			return handle, nil
		}
	}
	return nil, errors.New("retained reads require the actual owning native Tx or BEGIN IMMEDIATE Conn")
}

func readScopedRetainedOwnerTx(ctx context.Context, transaction db.Transaction) ([]db.ImportRecord, error) {
	// Preserve the exact owning Tx or leaseAttempt BEGIN IMMEDIATE Conn.
	q, err := scopedRetainedOwningReader(transaction)
	if err != nil {
		return nil, err
	}
	// This private adapter is ONLY the existing transaction-pinned export
	// reader. No domain/storage method or cached authority is invoked on it.
	reader := &Store{readQ: q}
	filter := db.ExportFilter{IncludeDeleted: true}
	var records []db.ImportRecord
	for value, err := range reader.ExportMeta(ctx) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportProjects(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportIssues(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportComments(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportEvents(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportCronJobs(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportCronWorkflows(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportCronRuns(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportFederationBindings(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportFederationEnrollments(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for value, err := range reader.ExportPendingClaimRequests(ctx, filter) {
		if err != nil {
			return nil, err
		}
		row := value
		records = append(records, &row)
	}
	for _, source := range []iter.Seq2[db.ImportRecord, error]{
		exportScopedOwnerAttributionTx(ctx, transaction),
		exportScopedOwnerArtifactsTx(ctx, transaction),
		reader.ExportRelayState(ctx),
		reader.ExportScopedState(ctx),
	} {
		for row, err := range source {
			if err != nil {
				return nil, err
			}
			records = append(records, row)
		}
	}
	// ONE existing validation entry path checks native receipt/ref/cron/relay
	// context and invokes the existing complete scoped graph validator. No
	// native SELECT/Scan graph validator or alternate admission core is added.
	if err := db.ValidateImportRecords(records); err != nil {
		return nil, err
	}
	for _, record := range records {
		if row, ok := record.(*db.ScopedSourceOperationExport); ok && row.RequestFingerprint != db.ScopedOfferedDigest(row.ManifestBytes) {
			return nil, db.ErrScopedSourceConflict
		}
	}
	return records, nil
}

func exportScopedOwnerAttributionTx(ctx context.Context, transaction db.Transaction, pinsOnly ...bool) iter.Seq2[db.ImportRecord, error] {
	return func(yield func(db.ImportRecord, error) bool) {
		q, err := scopedRetainedOwningReader(transaction)
		if err != nil {
			yield(nil, err)
			return
		}
		for _, kind := range []string{"federation_root_key", "federation_event_provenance", "federation_entity_provenance"} {
			if len(pinsOnly) != 0 && pinsOnly[0] && kind != "federation_root_key" {
				continue
			}
			var query string
			switch kind {
			case "federation_root_key":
				query = `SELECT k.project_uid,k.authority_uid,k.key_id,k.public_key,k.active FROM federation_root_keys k ORDER BY k.project_uid,k.key_id`
			case "federation_event_provenance":
				query = `SELECT k.receipt FROM federation_event_provenance k ORDER BY k.project_uid,k.reset_epoch,k.sequence`
			default:
				query = `SELECT k.project_uid,k.kind,k.entity_uid,k.event_uid FROM federation_entity_provenance k ORDER BY k.project_uid,k.kind,k.entity_uid`
			}
			rows, err := q.QueryContext(ctx, query)
			if err != nil {
				yield(nil, err)
				return
			}
			keep := true
			for rows.Next() {
				var record db.ImportRecord
				switch kind {
				case "federation_root_key":
					pin, e := scanRootPin(rows)
					if e != nil {
						yield(nil, e)
						_ = rows.Close()
						return
					}
					record = &pin
				case "federation_event_provenance":
					receipt, e := readReceipt(rows)
					if e != nil {
						yield(nil, e)
						_ = rows.Close()
						return
					}
					record = &receipt
				default:
					var ref db.EntityProvenance
					if e := rows.Scan(&ref.ProjectUID, &ref.Kind, &ref.EntityUID, &ref.EventUID); e != nil {
						yield(nil, e)
						_ = rows.Close()
						return
					}
					record = &ref
				}
				if !yield(record, nil) {
					keep = false
					break
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				yield(nil, fmt.Errorf("export root provenance: %w", err))
				return
			}
			if !keep {
				return
			}
		}
	}
}

func exportScopedOwnerArtifactsTx(ctx context.Context, transaction db.Transaction) iter.Seq2[db.ImportRecord, error] {
	const query = `SELECT a.project_uid,a.digest,a.artifact FROM federation_embedding_artifacts a JOIN projects p ON p.uid=a.project_uid JOIN issues i ON i.uid=a.issue_uid AND i.project_id=p.id WHERE a.staging_expires_at IS NULL ORDER BY a.project_uid,a.digest`
	scan := func(rows *sql.Rows) (db.ImportRecord, error) {
		var projectUID, digest, raw string
		if err := rows.Scan(&projectUID, &digest, &raw); err != nil {
			return nil, err
		}
		var record db.EmbeddingArtifactExport
		if err := json.Unmarshal([]byte(raw), &record.EmbeddingArtifact, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		if record.ProjectUID != projectUID || record.Digest != digest {
			return nil, db.ErrFederationIngestValidation
		}
		if err := embedding.ValidateArtifact(record.EmbeddingArtifact, "", ""); err != nil {
			return nil, err
		}
		return &record, nil
	}
	q, err := scopedRetainedOwningReader(transaction)
	if err != nil {
		return func(yield func(db.ImportRecord, error) bool) { yield(nil, err) }
	}
	return streamRows(ctx, q, "owner embedding artifacts", query, nil, scan)
}
