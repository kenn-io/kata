package pgstore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
)

func (s *Store) cronSQL() db.CronSQL {
	return db.CronSQL{CreateIssueTx: s.createIssueTx, PatchIssueMetadataTx: func(ctx context.Context, tx *sql.Tx, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
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
func (s *Store) RecurringCronDelivery(ctx context.Context, query db.CronDeliveryQuery, now time.Time) ([]db.CronDeliveryJob, error) {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	a := s.cronSQL()
	a.Query = tx
	a.RecurringChild = func(ctx context.Context, parentID int64, jobUID string) (*db.RecurringChild, error) {
		if s.uiReadStage != nil {
			if err := s.uiReadStage(ctx); err != nil {
				return nil, err
			}
		}
		return s.recurringChildTx(ctx, tx, parentID, jobUID)
	}
	out, err := a.RecurringDelivery(ctx, query, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) recurringChildTx(ctx context.Context, tx *sql.Tx, parentID int64, jobUID string) (*db.RecurringChild, error) {
	query, args := recurringChildQuery(ctx, parentID, jobUID)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return db.LastRecurringChild(ctx, rows)
}

func recurringChildQuery(ctx context.Context, parentID int64, jobUID string) (string, []any) {
	args := []any{jobUID, parentID}
	//nolint:gosec // The authorization predicate is generated from fixed SQL and bound parameters.
	query := `SELECT i.id, i.uid, i.metadata::jsonb -> 'cron.occurrence', kata_occurrence_key(i.metadata::json -> 'cron.occurrence') FROM issues i WHERE (i.metadata::jsonb -> 'cron.occurrence' ->> 'job_uid') COLLATE "C"=$1 AND i.deleted_at IS NULL AND kata_occurrence_key(i.metadata::json -> 'cron.occurrence') IS NOT NULL AND EXISTS (SELECT 1 FROM links l JOIN projects p ON p.id=i.project_id WHERE l.from_issue_id=i.id AND l.type='parent' AND l.to_issue_id=$2 AND p.deleted_at IS NULL AND ` + authorizedLinkPredicate(ctx, "l.", &args) + `) ORDER BY kata_occurrence_key(i.metadata::json -> 'cron.occurrence') COLLATE "C" DESC, i.uid COLLATE "C" DESC LIMIT 1`
	return query, args
}
