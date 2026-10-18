package pgstore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
)

func (s *Store) cronSQL() db.CronSQL {
	return db.CronSQL{RecurringChildren: s.recurringChildren, CreateIssueTx: s.createIssueTx, PatchIssueMetadataTx: func(ctx context.Context, tx *sql.Tx, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
		return s.patchIssueMetadata(ctx, in, func(_ context.Context, run transactionFunc) error { return run(tx) })
	}, Postgres: true, InstanceUID: s.InstanceUID(), Query: s, Transact: func(ctx context.Context, run func(*sql.Tx) error) error { return s.withSerializableTx(ctx, run) }, WriteGate: func(ctx context.Context, tx *sql.Tx, id int64) error { return ensureProjectWritableTx(ctx, tx, id) }, EffectiveActor: func(ctx context.Context, tx *sql.Tx, projectID int64, actor string) (string, error) {
		return effectiveLocalMutationActorTx(ctx, tx, projectID, actor)
	}, InsertEvent: func(ctx context.Context, tx *sql.Tx, event db.CronEvent) (db.Event, error) {
		return s.insertEventTx(ctx, tx, eventInsert{ProjectID: event.ProjectID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueID: event.IssueID, IssueUID: event.IssueUID, Type: event.Type, Actor: event.Actor, Payload: event.Payload})
	}}
}

// PutCronJob validates and writes an attributed job definition.
func (s *Store) PutCronJob(ctx context.Context, in db.PutCronJob) (db.CronJob, []db.Event, error) {
	return s.cronSQL().PutJob(ctx, in)
}

// PutCronWorkflow validates and writes an attributed workflow definition.
func (s *Store) PutCronWorkflow(ctx context.Context, in db.PutCronWorkflow) (db.CronWorkflow, db.Event, error) {
	return s.cronSQL().PutWorkflow(ctx, in)
}

// CronJob reads one project-scoped job definition.
func (s *Store) CronJob(ctx context.Context, project int64, id string) (db.CronJob, error) {
	return s.cronSQL().Job(ctx, project, id)
}

// CronWorkflow reads one project-scoped workflow definition.
func (s *Store) CronWorkflow(ctx context.Context, project int64, id string) (db.CronWorkflow, error) {
	return s.cronSQL().Workflow(ctx, project, id)
}

// ListCronJobs lists job definitions under the requested project filter.
func (s *Store) ListCronJobs(ctx context.Context, in db.CronList) ([]db.CronJob, error) {
	return s.cronSQL().Jobs(ctx, in)
}

// ListCronWorkflows lists workflow definitions under the requested project filter.
func (s *Store) ListCronWorkflows(ctx context.Context, in db.CronList) ([]db.CronWorkflow, error) {
	return s.cronSQL().Workflows(ctx, in)
}

// ListRecurringCronJobs returns lightweight native scheduling candidates.
func (s *Store) ListRecurringCronJobs(ctx context.Context, after string, limit int) ([]db.RecurringCronCandidate, error) {
	return s.cronSQL().RecurringJobs(ctx, after, limit)
}

// DispatchRecurringCron commits a bounded set of children and its progress together.
func (s *Store) DispatchRecurringCron(ctx context.Context, in db.DispatchRecurringCronIn) (db.DispatchRecurringCronOut, error) {
	return s.cronSQL().DispatchRecurring(ctx, in)
}

// RecurringCronDelivery reads status without changing delivery evidence.
func (s *Store) RecurringCronDelivery(ctx context.Context, projectID int64, jobs []db.CronJob, now time.Time) ([]*db.RecurringDelivery, error) {
	return s.cronSQL().RecurringDelivery(ctx, projectID, jobs, now)
}

func (s *Store) recurringChildren(ctx context.Context, parentID int64, jobUID string) ([]db.RecurringChild, error) {
	args := []any{parentID, jobUID}
	query := `SELECT i.uid, i.metadata::jsonb -> 'cron.occurrence' FROM issues i JOIN projects p ON p.id=i.project_id JOIN links l ON l.from_issue_id=i.id WHERE l.type='parent' AND l.to_issue_id=$1 AND i.metadata::jsonb -> 'cron.occurrence' ->> 'job_uid'=$2 AND i.deleted_at IS NULL AND p.deleted_at IS NULL AND ` + authorizedLinkPredicate(ctx, "l.", &args)
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []db.RecurringChild
	for rows.Next() {
		var uid, occurrence string
		if err := rows.Scan(&uid, &occurrence); err != nil {
			return nil, err
		}
		child, err := db.RecurringChildFrom(uid, occurrence)
		// Corrupt provenance is not proof of a delivered occurrence.
		if err == nil {
			out = append(out, child)
		}
	}
	return out, rows.Err()
}
