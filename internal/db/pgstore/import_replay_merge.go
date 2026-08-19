package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"go.kenn.io/kata/internal/db"
)

func (s *Store) prepareProjectMergeTx(
	ctx context.Context,
	tx *sql.Tx,
	records []db.ImportRecord,
) ([]db.ImportRecord, error) {
	if err := acquireExclusiveServingLease(ctx, tx, s.schema); err != nil {
		return nil, fmt.Errorf("quiesce serving daemons for project merge: %w", err)
	}
	if err := acquireSchemaMigrationLock(ctx, tx); err != nil {
		return nil, fmt.Errorf("lock schema migrations for project merge: %w", mapSQLError(err, nil))
	}
	tables, err := schemaTableNames(ctx, tx)
	if err != nil {
		return nil, err
	}
	quoted := make([]string, len(tables))
	for i, table := range tables {
		quoted[i] = quoteIdentifier(table)
	}
	if len(quoted) > 0 {
		if _, err := tx.ExecContext(ctx,
			`LOCK TABLE `+strings.Join(quoted, ", ")+` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return nil, fmt.Errorf("lock project merge target: %w", mapSQLError(err, nil))
		}
	}
	offsets, err := pgProjectMergeOffsets(ctx, tx)
	if err != nil {
		return nil, err
	}
	prepared, err := db.PrepareProjectMergeRecords(records, offsets,
		func(uid string) (int64, bool, error) {
			var id int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid=$1`, uid).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return 0, false, nil
			}
			return id, err == nil, mapSQLError(err, nil)
		})
	if err != nil {
		return nil, err
	}
	if err := refusePGProjectMergeUIDCollisions(ctx, tx, records); err != nil {
		return nil, err
	}
	return prepared, nil
}

func pgProjectMergeOffsets(ctx context.Context, tx *sql.Tx) (db.ProjectMergeOffsets, error) {
	var offsets db.ProjectMergeOffsets
	tables := []struct {
		name string
		dest *int64
	}{
		{"projects", &offsets.TargetProjectID},
		{"project_aliases", &offsets.Alias},
		{"issue_sync_bindings", &offsets.SyncBinding},
		{"recurrences", &offsets.Recurrence},
		{"issues", &offsets.Issue},
		{"comments", &offsets.Comment},
		{"links", &offsets.Link},
		{"import_mappings", &offsets.ImportMapping},
		{"federation_quarantine", &offsets.Quarantine},
		{"federation_enrollments", &offsets.Enrollment},
		{"issue_claims", &offsets.Claim},
		{"pending_claim_requests", &offsets.PendingClaim},
		{"events", &offsets.Event},
		{"purge_log", &offsets.PurgeLog},
		{"project_purge_log", &offsets.ProjectPurgeLog},
	}
	for _, table := range tables {
		highWater, err := pgIdentityHighWater(ctx, tx, table.name)
		if err != nil {
			return db.ProjectMergeOffsets{}, err
		}
		*table.dest = highWater
	}
	if offsets.TargetProjectID == math.MaxInt64 {
		return db.ProjectMergeOffsets{}, errors.New("project merge project ID overflows")
	}
	offsets.TargetProjectID++
	var resetHighWater int64
	if err := tx.QueryRowContext(ctx, `
		SELECT GREATEST(
			COALESCE((SELECT MAX(purge_reset_after_event_id) FROM purge_log), 0),
			COALESCE((SELECT MAX(purge_reset_after_event_id) FROM project_purge_log), 0)
		)`).Scan(&resetHighWater); err != nil {
		return db.ProjectMergeOffsets{}, fmt.Errorf("inspect project merge reset cursor: %w", mapSQLError(err, nil))
	}
	if resetHighWater > offsets.Event {
		offsets.Event = resetHighWater
	}
	return offsets, nil
}

func pgIdentityHighWater(ctx context.Context, tx *sql.Tx, table string) (int64, error) {
	query := `SELECT GREATEST(
		(SELECT COALESCE(MAX(id), 0) FROM ` + quoteIdentifier(table) + `),
		COALESCE((
			SELECT s.last_value
			FROM pg_sequences s
			WHERE s.schemaname = current_schema()
			  AND to_regclass(format('%I.%I', s.schemaname, s.sequencename)) =
			      to_regclass(pg_get_serial_sequence(format('%I.%I', current_schema(), $1::text), 'id'))
		), 0)
	)`
	var highWater int64
	if err := tx.QueryRowContext(ctx, query, table).Scan(&highWater); err != nil {
		return 0, fmt.Errorf("inspect project merge %s ID range: %w", table, mapSQLError(err, nil))
	}
	return highWater, nil
}

func refusePGProjectMergeUIDCollisions(ctx context.Context, tx *sql.Tx, records []db.ImportRecord) error {
	type uidCheck struct{ table, column, kind, uid string }
	checks := make([]uidCheck, 0, len(records))
	for _, record := range records {
		switch {
		case record.Project != nil:
			checks = append(checks, uidCheck{"projects", "uid", "project", record.Project.UID})
		case record.Issue != nil:
			checks = append(checks, uidCheck{"issues", "uid", "issue", record.Issue.UID})
		case record.Comment != nil:
			checks = append(checks, uidCheck{"comments", "uid", "comment", record.Comment.UID})
		case record.Recurrence != nil:
			checks = append(checks, uidCheck{"recurrences", "uid", "recurrence", record.Recurrence.UID})
		case record.IssueClaim != nil:
			checks = append(checks, uidCheck{"issue_claims", "claim_uid", "claim", record.IssueClaim.ClaimUID})
		case record.PendingClaimRequest != nil:
			checks = append(checks, uidCheck{"pending_claim_requests", "request_uid", "pending claim", record.PendingClaimRequest.RequestUID})
		case record.Event != nil:
			checks = append(checks, uidCheck{"events", "uid", "event", record.Event.UID})
		case record.PurgeLog != nil:
			checks = append(checks, uidCheck{"purge_log", "uid", "purge log", record.PurgeLog.UID})
		}
	}
	for _, check := range checks {
		if check.uid == "" {
			continue
		}
		query := `SELECT EXISTS (SELECT 1 FROM ` + quoteIdentifier(check.table) +
			` WHERE ` + quoteIdentifier(check.column) + `=$1)`
		var exists bool
		if err := tx.QueryRowContext(ctx, query, check.uid).Scan(&exists); err != nil {
			return fmt.Errorf("check project merge %s UID: %w", check.kind, mapSQLError(err, nil))
		}
		if exists {
			return fmt.Errorf("project merge refused: %s UID %q already exists", check.kind, check.uid)
		}
	}
	return nil
}
