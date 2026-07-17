package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// ImportReplay atomically restores normalized JSONL records into Postgres.
// Explicit source identities are preserved, identity sequences are advanced,
// and the token projection is rebuilt from its authoritative events.
func (s *Store) ImportReplay(ctx context.Context, records []db.ImportRecord, opts db.ImportOptions) error {
	if err := db.ValidateImportRecords(records); err != nil {
		return err
	}
	var finalInstanceUID string
	err := s.withTx(ctx, sql.LevelReadCommitted, func(tx *sql.Tx) error {
		if err := s.importReplayTx(ctx, tx, db.OrderImportRecords(records), opts); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&finalInstanceUID); err != nil {
			return fmt.Errorf("read restored instance_uid: %w", mapSQLError(err, nil))
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.instanceUID = finalInstanceUID
	return nil
}

type replayLinkSkip int

const (
	replayLinkInserted replayLinkSkip = iota
	replayLinkMissingPeer
	replayLinkDuplicate
	replayLinkMapping
)

func (s *Store) importReplayTx(
	ctx context.Context,
	tx *sql.Tx,
	records []db.ImportRecord,
	opts db.ImportOptions,
) error {
	preservedInstanceUID, err := s.pgReplayClearTarget(ctx, tx, opts)
	if err != nil {
		return err
	}

	var missingPeers, duplicates, skippedMappings int
	skippedLinkIDs := make(map[int64]struct{})
	sequenceFloors := make(map[string]int64)
	for _, record := range records {
		skip, err := s.importReplayRecord(ctx, tx, record, opts, skippedLinkIDs, sequenceFloors)
		if err != nil {
			return err
		}
		switch skip {
		case replayLinkMissingPeer:
			missingPeers++
			if record.Link != nil {
				skippedLinkIDs[record.Link.ID] = struct{}{}
			}
		case replayLinkDuplicate:
			duplicates++
		case replayLinkMapping:
			skippedMappings++
		}
	}
	if missingPeers > 0 {
		fmt.Fprintf(os.Stderr,
			"note: skipped %d link record(s) whose peer issue is not in this envelope or database\n",
			missingPeers)
	}
	if duplicates > 0 {
		fmt.Fprintf(os.Stderr,
			"note: skipped %d duplicate link record(s) (edge already present)\n",
			duplicates)
	}
	if skippedMappings > 0 {
		fmt.Fprintf(os.Stderr,
			"note: skipped %d import mapping record(s) referencing skipped link(s)\n",
			skippedMappings)
	}

	if err := pgReplayEnsureSystemProject(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('instance_uid',$1) ON CONFLICT(key) DO NOTHING`,
		preservedInstanceUID); err != nil {
		return fmt.Errorf("restore target instance_uid: %w", mapSQLError(err, nil))
	}
	if err := s.replayAPITokens(ctx, tx); err != nil {
		return err
	}
	if err := pgReplayRecordSchemaVersion(ctx, tx); err != nil {
		return err
	}
	if err := s.reconcileReplayIdentities(ctx, tx, sequenceFloors); err != nil {
		return err
	}
	return nil
}

// pgReplayClearTarget makes replay an atomic whole-schema replacement. The
// table inventory comes from the selected schema, so future migration-owned
// tables participate without a second hand-maintained list. NewInstance keeps
// the target identity in memory while meta is replaced with the snapshot.
func (s *Store) pgReplayClearTarget(
	ctx context.Context,
	tx *sql.Tx,
	opts db.ImportOptions,
) (string, error) {
	if err := acquireExclusiveServingLease(ctx, tx, s.schema); err != nil {
		return "", fmt.Errorf("quiesce serving daemons for import: %w", err)
	}
	if err := acquireSchemaMigrationLock(ctx, tx); err != nil {
		return "", fmt.Errorf("lock schema migrations for import: %w", mapSQLError(err, nil))
	}
	allTables, err := schemaTableNames(ctx, tx)
	if err != nil {
		return "", err
	}
	if len(allTables) == 0 {
		return "", nil
	}
	quotedAll := make([]string, len(allTables))
	for i, table := range allTables {
		quotedAll[i] = quoteIdentifier(table)
	}
	if _, err := tx.ExecContext(ctx,
		`LOCK TABLE `+strings.Join(quotedAll, ", ")+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		return "", fmt.Errorf("lock import target tables: %w", mapSQLError(err, nil))
	}
	if opts.RequireFreshTarget {
		if err := validateFreshSchema(ctx, tx, allTables, s.instanceUID); err != nil {
			return "", fmt.Errorf("import requires a fresh target: %w", err)
		}
	}
	var preservedInstanceUID string
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&preservedInstanceUID); err != nil {
		return "", fmt.Errorf("preserve target instance_uid: %w", mapSQLError(err, nil))
	}
	if _, err := tx.ExecContext(ctx,
		`TRUNCATE TABLE `+strings.Join(quotedAll, ", ")+` RESTART IDENTITY`); err != nil {
		return "", fmt.Errorf("clear import target: %w", mapSQLError(err, nil))
	}
	return preservedInstanceUID, nil
}

func (s *Store) importReplayRecord(
	ctx context.Context,
	tx *sql.Tx,
	record db.ImportRecord,
	opts db.ImportOptions,
	skippedLinkIDs map[int64]struct{},
	sequenceFloors map[string]int64,
) (replayLinkSkip, error) {
	switch record.Kind {
	case db.ImportKindMeta:
		return replayLinkInserted, pgReplayMeta(ctx, tx, record.Meta, opts)
	case db.ImportKindProject:
		return replayLinkInserted, pgReplayProject(ctx, tx, record.Project)
	case db.ImportKindProjectAlias:
		return replayLinkInserted, pgReplayAlias(ctx, tx, record.Alias)
	case db.ImportKindIssueSyncBinding:
		return replayLinkInserted, pgReplayIssueSyncBinding(
			ctx, tx, record.IssueSyncBinding, opts.PreserveIssueSyncBindingEnabled,
		)
	case db.ImportKindIssueSyncStatus:
		return replayLinkInserted, pgReplayIssueSyncStatus(ctx, tx, record.IssueSyncStatus)
	case db.ImportKindRecurrence:
		return replayLinkInserted, pgReplayRecurrence(ctx, tx, record.Recurrence)
	case db.ImportKindIssue:
		return replayLinkInserted, pgReplayIssue(ctx, tx, record.Issue)
	case db.ImportKindIssueEmbedding:
		return replayLinkInserted, nil
	case db.ImportKindComment:
		return replayLinkInserted, pgReplayComment(ctx, tx, record.Comment)
	case db.ImportKindIssueLabel:
		return replayLinkInserted, pgReplayLabel(ctx, tx, record.Label)
	case db.ImportKindLink:
		return pgReplayLink(ctx, tx, record.Link)
	case db.ImportKindImportMapping:
		return pgReplayImportMapping(ctx, tx, record.ImportMapping, skippedLinkIDs)
	case db.ImportKindFederationBinding:
		return replayLinkInserted, pgReplayFederationBinding(ctx, tx, record.FederationBinding)
	case db.ImportKindFederationSyncStatus:
		return replayLinkInserted, pgReplayFederationSyncStatus(ctx, tx, record.FederationSyncStatus)
	case db.ImportKindFederationQuarantine:
		return replayLinkInserted, pgReplayFederationQuarantine(ctx, tx, record.FederationQuarantine)
	case db.ImportKindFederationEnrollment:
		return replayLinkInserted, pgReplayFederationEnrollment(ctx, tx, record.FederationEnrollment)
	case db.ImportKindIssueClaim:
		return replayLinkInserted, pgReplayIssueClaim(ctx, tx, record.IssueClaim)
	case db.ImportKindPendingClaimRequest:
		return replayLinkInserted, pgReplayPendingClaim(ctx, tx, record.PendingClaimRequest, opts)
	case db.ImportKindEvent:
		return replayLinkInserted, pgReplayEvent(ctx, tx, record.Event, opts)
	case db.ImportKindPurgeLog:
		return replayLinkInserted, pgReplayPurgeLog(ctx, tx, record.PurgeLog)
	case db.ImportKindProjectPurgeLog:
		return replayLinkInserted, pgReplayProjectPurgeLog(ctx, tx, record.ProjectPurgeLog)
	case db.ImportKindSQLiteSequence:
		if record.Sequence.Seq > sequenceFloors[record.Sequence.Name] {
			sequenceFloors[record.Sequence.Name] = record.Sequence.Seq
		}
		return replayLinkInserted, nil
	default:
		return replayLinkInserted, fmt.Errorf("import: unsupported kind %q", record.Kind)
	}
}

func pgReplayError(kind string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("import %s: %w", kind, mapSQLError(err, nil))
}
