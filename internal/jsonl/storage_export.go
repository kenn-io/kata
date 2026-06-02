package jsonl

import (
	"context"
	"fmt"
	"io"
	"iter"
	"strconv"

	"go.kenn.io/kata/internal/db"
)

// exportSnapshotter is implemented by stores (today: *sqlitestore.Store) that
// can pin all iterator reads to a single read-only transaction. When present,
// Export uses it so a concurrent writer that commits mid-export does not bleed
// rows into the output.
type exportSnapshotter interface {
	BeginExportSnapshot(ctx context.Context) (db.Storage, func() error, error)
}

// Export writes a deterministic JSONL export of store to w. Current-schema
// exports are backend-neutral and route reads through db.Storage iterator
// methods. Pre-current SQLite stores route through the version-aware legacy
// exporter so read-only backups of old DBs do not depend on current columns.
func Export(ctx context.Context, store db.Storage, w io.Writer, opts ExportOptions) error {
	v, err := store.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if v < db.CurrentSchemaVersion() {
		q, ok := store.(exportQuerier)
		if !ok {
			return fmt.Errorf("export schema_version %d requires a version-aware SQLite exporter", v)
		}
		return exportForCutover(ctx, q, w, opts)
	}

	if snap, ok := store.(exportSnapshotter); ok {
		snapshot, release, err := snap.BeginExportSnapshot(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = release() }()
		store = snapshot
	}
	enc := NewEncoder(w)
	f := db.ExportFilter{IncludeDeleted: opts.IncludeDeleted}
	if opts.ProjectID > 0 {
		f.ProjectID = &opts.ProjectID
	}

	// export_version mirrors the DB's stored schema_version, matching the
	// old behavior (which read meta.schema_version directly).
	if err := writeRecord(enc, KindMeta, metaRecord{Key: "export_version", Value: strconv.Itoa(v)}); err != nil {
		return err
	}

	if err := streamExport(enc, KindMeta, store.ExportMeta(ctx)); err != nil {
		return err
	}
	if err := streamExport(enc, KindProject, store.ExportProjects(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindProjectAlias, store.ExportProjectAliases(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindRecurrence, store.ExportRecurrences(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindIssue, store.ExportIssues(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindComment, store.ExportComments(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindIssueLabel, store.ExportIssueLabels(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindLink, store.ExportLinks(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindImportMapping, store.ExportImportMappings(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindFederationBinding, store.ExportFederationBindings(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindFederationSyncStatus, store.ExportFederationSyncStatus(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindFederationQuarantine, store.ExportFederationQuarantine(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindFederationEnrollment, store.ExportFederationEnrollments(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindIssueClaim, store.ExportIssueClaims(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindPendingClaimRequest, store.ExportPendingClaimRequests(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindEvent, store.ExportEvents(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindPurgeLog, store.ExportPurgeLog(ctx, f)); err != nil {
		return err
	}
	return streamExport(enc, KindSQLiteSequence, store.ExportSequences(ctx))
}

// streamExport ranges seq and writes each row as a kind-tagged envelope to enc.
func streamExport[T any](enc *Encoder, kind Kind, seq iter.Seq2[T, error]) error {
	for rec, err := range seq {
		if err != nil {
			return err
		}
		if err := writeRecord(enc, kind, rec); err != nil {
			return err
		}
	}
	return nil
}
