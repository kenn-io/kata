package sqlitestore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) cronSQL() db.CronSQL {
	return db.CronSQL{InstanceUID: d.InstanceUID(), Query: d, Transact: func(ctx context.Context, run func(*sql.Tx) error) error {
		return d.RetryTransient(ctx, func() error {
			tx, err := d.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := run(tx); err != nil {
				return err
			}
			return tx.Commit()
		})
	}, WriteGate: func(ctx context.Context, tx *sql.Tx, id int64) error { return ensureProjectWritableTx(ctx, tx, id) }, EffectiveActor: func(ctx context.Context, tx *sql.Tx, projectID int64, actor string) (string, error) {
		return d.effectiveLocalMutationActorTx(ctx, tx, projectID, actor)
	}, InsertEvent: func(ctx context.Context, tx *sql.Tx, event db.CronEvent) (db.Event, error) {
		return d.insertEventTx(ctx, tx, eventInsert{ProjectID: event.ProjectID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueID: event.IssueID, IssueUID: event.IssueUID, Type: event.Type, Actor: event.Actor, Payload: event.Payload})
	}}
}

// PutCronJob validates and writes an attributed job definition.
func (d *Store) PutCronJob(ctx context.Context, in db.PutCronJob) (db.CronJob, []db.Event, error) {
	return d.cronSQL().PutJob(ctx, in)
}

// PutCronWorkflow validates and writes an attributed workflow definition.
func (d *Store) PutCronWorkflow(ctx context.Context, in db.PutCronWorkflow) (db.CronWorkflow, db.Event, error) {
	return d.cronSQL().PutWorkflow(ctx, in)
}

// CronJob reads one project-scoped job definition.
func (d *Store) CronJob(ctx context.Context, project int64, id string) (db.CronJob, error) {
	return d.cronSQL().Job(ctx, project, id)
}

// CronWorkflow reads one project-scoped workflow definition.
func (d *Store) CronWorkflow(ctx context.Context, project int64, id string) (db.CronWorkflow, error) {
	return d.cronSQL().Workflow(ctx, project, id)
}

// ListCronJobs lists job definitions under the requested project filter.
func (d *Store) ListCronJobs(ctx context.Context, in db.CronList) ([]db.CronJob, error) {
	return d.cronSQL().Jobs(ctx, in)
}

// ListCronWorkflows lists workflow definitions under the requested project filter.
func (d *Store) ListCronWorkflows(ctx context.Context, in db.CronList) ([]db.CronWorkflow, error) {
	return d.cronSQL().Workflows(ctx, in)
}
