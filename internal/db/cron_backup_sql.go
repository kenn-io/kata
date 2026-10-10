package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"iter"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// CronQuery is supplied by the backend's pinned export snapshot.
type CronQuery func(context.Context, string, ...any) (*sql.Rows, error)

func cronExport[T any](ctx context.Context, query CronQuery, filter ExportFilter, table, columns string, hasDeletedAt bool, scan func(*sql.Rows) (T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		statement := "SELECT " + columns + " FROM " + table + " WHERE 1=1"
		if !filter.IncludeDeleted {
			statement += " AND project_id IN (SELECT id FROM projects WHERE deleted_at IS NULL)"
			if hasDeletedAt {
				statement += " AND deleted_at IS NULL"
			}
		}
		var args []any
		if filter.ProjectID != nil {
			statement += " AND project_id=$1"
			args = append(args, *filter.ProjectID)
		}
		statement += " ORDER BY 1"
		rows, err := query(ctx, statement, args...)
		if err != nil {
			yield(zero, err)
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			value, err := scan(rows)
			if !yield(value, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}

type cronTime struct{ target *time.Time }

func (s cronTime) Scan(value any) error {
	if v, ok := value.(time.Time); ok {
		*s.target = v.UTC()
		return nil
	}
	var raw string
	switch v := value.(type) {
	case string:
		raw = v
	case []byte:
		raw = string(v)
	default:
		return fmt.Errorf("invalid cron timestamp type %T", value)
	}
	v, err := time.Parse(time.RFC3339Nano, raw)
	if err == nil {
		*s.target = v
	}
	return err
}

type cronOptionalTime struct{ target **time.Time }

func (s cronOptionalTime) Scan(value any) error {
	if value == nil {
		*s.target = nil
		return nil
	}
	var parsed time.Time
	if err := (cronTime{&parsed}).Scan(value); err != nil {
		return err
	}
	*s.target = &parsed
	return nil
}
func cronTimeValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

const cronRunColumns = `id,uid,project_id,job_uid,definition_event_uid,workflow_uid,workflow_definition_event_uid,occurrence_key,issue_uid,actor,teammate,executor_label,status,summary_json,revision,created_at,started_at,ended_at,updated_at`

// ExportCronRunsSQL streams attributed observations within the export scope.
func ExportCronRunsSQL(ctx context.Context, query CronQuery, filter ExportFilter) iter.Seq2[CronRunExport, error] {
	return cronExport(ctx, query, filter, "cron_runs", cronRunColumns, false, scanCronRun)
}
func scanCronRun(row *sql.Rows) (CronRunExport, error) { return scanCronRunRow(row) }
func scanCronRunRow(row interface{ Scan(...any) error }) (CronRunExport, error) {
	var value CronRunExport
	var summary string
	if err := row.Scan(&value.ID, &value.UID, &value.ProjectID, &value.JobUID, &value.DefinitionEventUID, &value.WorkflowUID, &value.WorkflowDefinitionEventUID, &value.OccurrenceKey, &value.IssueUID, &value.Actor, &value.Teammate, &value.ExecutorLabel, &value.Status, &summary, &value.Revision, cronTime{&value.CreatedAt}, cronOptionalTime{&value.StartedAt}, cronOptionalTime{&value.EndedAt}, cronTime{&value.UpdatedAt}); err != nil {
		return value, err
	}
	var err error
	value.Summary, err = cron.ParseSummary([]byte(summary))
	return value, err
}
func replayCronRun(ctx context.Context, tx *sql.Tx, value *CronRunExport, postgres bool) error {
	return insertCronRun(ctx, tx, CronRun(*value), postgres, true)
}
func insertCronRun(ctx context.Context, tx *sql.Tx, value CronRun, postgres, withID bool) error {
	summary, err := json.Marshal(value.Summary)
	if err != nil {
		return err
	}
	columns := cronRunColumns
	args := []any{value.ID, value.UID, value.ProjectID, value.JobUID, value.DefinitionEventUID, value.WorkflowUID, value.WorkflowDefinitionEventUID, value.OccurrenceKey, value.IssueUID, value.Actor, value.Teammate, value.ExecutorLabel, value.Status, string(summary), value.Revision, value.CreatedAt.UTC().Format(CronRunCreatedAtFormat), cronTimeValue(value.StartedAt), cronTimeValue(value.EndedAt), value.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	override := ""
	if !withID {
		columns = strings.TrimPrefix(columns, "id,")
		args = args[1:]
	} else if postgres {
		override = " OVERRIDING SYSTEM VALUE"
	}
	slots := make([]string, len(args))
	for i := range slots {
		slots[i] = fmt.Sprintf("$%d", i+1)
	}
	// A concurrent writer can claim the same fresh UID in another project after
	// the caller's existence check. Report that as a conflict, not a raw
	// constraint failure. Restore inserts keep their row IDs and run alone.
	conflict := ""
	if !withID {
		conflict = " ON CONFLICT(uid) DO NOTHING"
	}
	inserted, err := tx.ExecContext(ctx, "INSERT INTO cron_runs("+columns+")"+override+" VALUES("+strings.Join(slots, ",")+")"+conflict, args...) //nolint:gosec // Only fixed schema columns, Boolean-selected tables/identity syntax and generated placeholders enter SQL; row values are bound.
	if err != nil || withID {
		return err
	}
	return requireOneCronRow(inserted)
}

// requireOneCronRow turns an insert or compare-and-swap that changed no row
// into ErrCronConflict.
func requireOneCronRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrCronConflict
	}
	return nil
}

// ExportCronJobsSQL streams job documents with their winning provenance.
func ExportCronJobsSQL(ctx context.Context, query CronQuery, filter ExportFilter) iter.Seq2[CronJobExport, error] {
	return cronExport(ctx, query, filter, "cron_jobs", cronDefinitionColumns, true, func(rows *sql.Rows) (CronJobExport, error) {
		row, err := scanCron(rows)
		if err != nil {
			return CronJobExport{}, err
		}
		value, err := jobFromRow(row)
		return CronJobExport(value), err
	})
}

// ExportCronWorkflowsSQL streams workflow documents with their winning provenance.
func ExportCronWorkflowsSQL(ctx context.Context, query CronQuery, filter ExportFilter) iter.Seq2[CronWorkflowExport, error] {
	return cronExport(ctx, query, filter, "cron_workflows", cronDefinitionColumns, true, func(rows *sql.Rows) (CronWorkflowExport, error) {
		row, err := scanCron(rows)
		if err != nil {
			return CronWorkflowExport{}, err
		}
		value, err := workflowFromRow(row)
		return CronWorkflowExport(value), err
	})
}

// ReplayCronRecord is called inside the backend's ordinary restore
// transaction. No execution permission is stored or restored.
func ReplayCronRecord(ctx context.Context, tx *sql.Tx, record ImportRecord, postgres bool) error {
	switch value := record.(type) {
	case *CronRunExport:
		return replayCronRun(ctx, tx, value, postgres)
	case *CronJobExport:
		return replayCronDefinition(ctx, tx, value.CronDefinition, value.Definition, true, postgres)
	case *CronWorkflowExport:
		return replayCronDefinition(ctx, tx, value.CronDefinition, value.Definition, false, postgres)
	default:
		return fmt.Errorf("unsupported cron record %T", record)
	}
}
func replayCronDefinition(ctx context.Context, tx *sql.Tx, value CronDefinition, definition any, job bool, postgres bool) error {
	document, err := json.Marshal(definition)
	if err != nil {
		return err
	}
	clock, err := json.Marshal(value.DefinitionHLC)
	if err != nil {
		return err
	}
	columns := cronDefinitionColumns
	args := []any{value.ID, value.UID, value.ProjectID, value.Name, string(document), value.DefinitionEventUID, string(clock), value.Author, value.Revision, value.CreatedAt.UTC().Format(time.RFC3339Nano), value.UpdatedAt.UTC().Format(time.RFC3339Nano), cronTimeValue(value.DeletedAt)}
	slots := make([]string, len(args))
	for i := range slots {
		slots[i] = fmt.Sprintf("$%d", i+1)
	}
	override := ""
	if postgres {
		override = " OVERRIDING SYSTEM VALUE"
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO "+cronTable(job)+"("+columns+")"+override+" VALUES("+strings.Join(slots, ",")+")", args...) //nolint:gosec // Only fixed schema columns, Boolean-selected tables/identity syntax and generated placeholders enter SQL; row values are bound.
	return err
}
