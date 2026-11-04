package db

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/cron"
)

type recurringGate struct {
	reason  cron.DispatchReason
	project Project
	job     CronJob
	parent  Issue
}

// recurringGateTx preserves dispatch's project/binding/job/parent precedence.
// An empty reason means planning can proceed under the existing write locks.
func (a CronSQL) recurringGateTx(ctx context.Context, tx *sql.Tx, in DispatchRecurringCronIn, dispatch bool) (recurringGate, error) {
	g := recurringGate{}
	if dispatch {
		if err := LockCronProject(ctx, tx, in.ProjectID); errors.Is(err, ErrNotFound) {
			g.reason = cron.DispatchProjectUnavailable
			return g, nil
		} else if err != nil {
			return g, err
		}
	}
	authority, role, err := recurringAuthority(ctx, tx, in.ProjectID)
	if err != nil {
		return g, err
	}
	if !authority {
		g.reason = cron.DispatchReplica
		if role == FederationRoleHub {
			g.reason = cron.DispatchBindingDisabled
		}
		return g, nil
	}

	if dispatch {
		g.project, err = a.openCronProject(ctx, tx, in.ProjectID)
	} else {
		g.project, err = a.cronProjectRow(ctx, tx, in.ProjectID)
	}
	if errors.Is(err, ErrNotFound) {
		g.reason = cron.DispatchProjectUnavailable
		return g, nil
	}
	if err != nil {
		return g, err
	}
	row, err := scanCron(tx.QueryRowContext(ctx, `SELECT `+cronDefinitionColumns+` FROM cron_jobs WHERE project_id=$1 AND uid=$2`, in.ProjectID, in.JobUID))
	if errors.Is(err, ErrNotFound) {
		g.reason = cron.DispatchJobMissing
		return g, nil
	}
	if err != nil {
		return g, err
	}
	if row.DeletedAt != nil {
		g.reason = cron.DispatchDeleted
		return g, nil
	}
	if dispatch && row.DefinitionEventUID != in.ExpectedEventUID {
		g.reason = cron.DispatchDefinitionChanged
		return g, nil
	}
	g.job, err = jobFromRow(row)
	if err != nil {
		g.reason = cron.DispatchScheduleInvalid
		return g, err
	}
	if !cron.IsRecurringIssueJob(g.job.Definition) {
		g.reason = cron.DispatchExternalAdapter
		return g, nil
	}
	if !g.job.Definition.Enabled {
		g.reason = cron.DispatchDisabled
		return g, nil
	}
	var deleted bool
	g.parent, deleted, err = a.recurringParentTx(ctx, tx, g.project.ID, g.job.Definition.Issue.ParentUID, dispatch)
	if errors.Is(err, ErrNotFound) {
		g.reason = cron.DispatchParentMissing
		return g, nil
	}
	if err != nil {
		return g, err
	}
	if deleted {
		g.reason = cron.DispatchParentDeleted
	} else if g.parent.Status != "open" {
		g.reason = cron.DispatchParentClosed
	}
	return g, nil
}
