package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

func (s *Store) cronSQL() db.CronSQL {
	return db.CronSQL{Postgres: true, InstanceUID: s.InstanceUID(), Query: s, Transact: func(ctx context.Context, run func(*sql.Tx) error) error { return s.withSerializableTx(ctx, run) }, WriteGate: func(ctx context.Context, tx *sql.Tx, id int64) error { return ensureProjectWritableTx(ctx, tx, id) }, EffectiveActor: func(ctx context.Context, tx *sql.Tx, projectID int64, actor string) (string, error) {
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
