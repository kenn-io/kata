package sqlitestore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) cronSQL() db.CronSQL {
	return db.CronSQL{RecurringChildren: d.recurringChildren, CreateIssueTx: func(ctx context.Context, tx *sql.Tx, p db.CreateIssueParams) (db.Issue, db.Event, error) {
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

// RecurringCronDelivery reads status without changing delivery evidence.
func (d *Store) RecurringCronDelivery(ctx context.Context, projectID int64, jobs []db.CronJob, now time.Time) ([]*db.RecurringDelivery, error) {
	return d.cronSQL().RecurringDelivery(ctx, projectID, jobs, now)
}

func (d *Store) recurringChildren(ctx context.Context, parentID int64, jobUID string) ([]db.RecurringChild, error) {
	args := []any{parentID, jobUID}
	query := `SELECT i.uid, json_extract(i.metadata,'$."cron.occurrence"') FROM issues i JOIN projects p ON p.id=i.project_id JOIN links l ON l.from_issue_id=i.id WHERE l.type='parent' AND l.to_issue_id=? AND json_extract(i.metadata,'$."cron.occurrence".job_uid')=? AND i.deleted_at IS NULL AND p.deleted_at IS NULL AND ` + authorizedLinkPredicate(ctx, "l.", &args)
	rows, err := d.QueryContext(ctx, query, args...)
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
