package sqlitestore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) cronSQL() db.CronSQL {
	return db.CronSQL{CreateIssueTx: func(ctx context.Context, tx *sql.Tx, p db.CreateIssueParams) (db.Issue, db.Event, error) {
		id, event, err := d.createIssueTx(ctx, tx, p)
		if err != nil {
			return db.Issue{}, db.Event{}, err
		}
		issue, err := issueByIDTx(ctx, tx, id)
		return issue, event, err
	}, PatchIssueMetadataTx: d.patchIssueMetadataTx, InstanceUID: d.InstanceUID(), Query: d, Transact: func(ctx context.Context, run func(*sql.Tx) error) error {
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

// ListRecurringCronJobs returns lightweight native scheduling candidates.
func (d *Store) ListRecurringCronJobs(ctx context.Context, after string, limit int) ([]db.RecurringCronCandidate, error) {
	return d.cronSQL().RecurringJobs(ctx, after, limit)
}

// DispatchRecurringCron commits a bounded set of children and its progress together.
func (d *Store) DispatchRecurringCron(ctx context.Context, in db.DispatchRecurringCronIn) (db.DispatchRecurringCronOut, error) {
	return d.cronSQL().DispatchRecurring(ctx, in)
}

// SetCronProjectPause changes root-owned policy in the cron project transaction.
func (d *Store) SetCronProjectPause(ctx context.Context, in db.SetCronProjectPauseIn) (db.PatchProjectMetadataOut, error) {
	return d.cronSQL().SetProjectPause(ctx, in)
}

// RequestCron admits a root request or returns an immutable exact replay.
func (d *Store) RequestCron(ctx context.Context, in db.RequestCronIn) (db.RequestCronOut, error) {
	return d.cronSQL().Request(ctx, in)
}

// ListCronRequests derives state from the local consistent evidence snapshot.
func (d *Store) ListCronRequests(ctx context.Context, in db.CronRequestList) ([]db.CronRequest, error) {
	return d.cronSQL().Requests(ctx, in)
}

// ListCronRequestProjects lists expiry candidates for per-project cleanup.
func (d *Store) ListCronRequestProjects(ctx context.Context) ([]int64, error) {
	return d.cronSQL().RequestProjects(ctx)
}

// SweepCronRequests returns the exact committed event, including on partial cleanup.
func (d *Store) SweepCronRequests(ctx context.Context, id int64, now time.Time) (*db.Event, error) {
	return d.cronSQL().SweepRequests(ctx, id, now)
}

// ResumeCronJob enables a job with an optional retained delivery cutoff.
func (d *Store) ResumeCronJob(ctx context.Context, in db.ResumeCronJobIn) (db.ResumeCronJobOut, error) {
	return d.cronSQL().ResumeJob(ctx, in)
}

// CronJobControlRead returns definition and effective cutoff in one snapshot.
func (d *Store) CronJobControlRead(ctx context.Context, pid int64, id string) (db.CronJobControlRead, error) {
	return d.cronSQL().JobControlRead(ctx, pid, id)
}

// ListCronJobControlReads returns consistent definition/cutoff pairs.
func (d *Store) ListCronJobControlReads(ctx context.Context, pid int64, includeDeleted bool) ([]db.CronJobControlRead, error) {
	return d.cronSQL().JobControlReads(ctx, pid, includeDeleted)
}
