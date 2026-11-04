package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// RecurringExplanation describes live eligibility, never a committed delivery.
// NextDueAt is the first schedule instant after stored progress, not a forecast
// of the child selected by catch-up. Detail is always a public fixed message.
type RecurringExplanation struct {
	Reason    cron.DispatchReason `json:"reason"`
	NextDueAt *time.Time          `json:"next_due_at,omitempty"`
	Detail    string              `json:"detail,omitempty"`
}

// ExplainRecurring must run in a backend's consistent read-only transaction.
// It shares the dispatch gates but omits write locks and mutation authority.
func (a CronSQL) ExplainRecurring(ctx context.Context, projectID int64, jobUID string, now time.Time) (out RecurringExplanation, err error) {
	defer func() {
		if err != nil && out.Reason == "" {
			out.Reason = cron.DispatchError
		}
		out.Detail = out.Reason.PublicDetail()
	}()
	tx, err := a.BeginRead(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	in := DispatchRecurringCronIn{ProjectID: projectID, JobUID: jobUID, Now: now}
	in.JobUID = strings.ToUpper(strings.TrimSpace(in.JobUID))
	// Establish membership in this same snapshot even when authority gates stop
	// evaluation early. A separate handler read can race a move or deletion.
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cron_jobs WHERE project_id=$1 AND uid=$2)`, projectID, in.JobUID).Scan(&exists); err != nil {
		return out, err
	}
	_, err = a.cronProjectRow(ctx, tx, projectID)
	if errors.Is(err, ErrNotFound) {
		out.Reason = cron.DispatchProjectUnavailable
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if !exists {
		out.Reason = cron.DispatchJobMissing
		return out, nil
	}
	g, err := a.recurringGateTx(ctx, tx, in, false)
	out.Reason = g.reason
	if err != nil || g.reason != "" {
		return out, err
	}
	progress, err := readRecurringProgress(g.parent, g.job.UID)
	if err != nil {
		out.Reason = cron.DispatchProgressInvalid
		return out, err
	}
	batch, err := cron.PlanRecurring(g.job.Definition, g.job.DefinitionEventUID, progress, in.Now)
	if err != nil {
		out.Reason = cron.DispatchScheduleInvalid
		return out, err
	}
	if _, err = recurringRecipient(g.job, g.parent); err != nil {
		out.Reason = cron.DispatchRecipientInvalid
		return out, err
	}
	next, err := cron.RecurringOccurrences(g.job.Definition, progress, progress.Through, 1)
	if err != nil {
		out.Reason = cron.DispatchScheduleInvalid
		return out, err
	}
	if len(next) == 0 {
		out.Reason = cron.DispatchScheduleInvalid
		return out, fmt.Errorf("%w: schedule has no supported next occurrence", cron.ErrInvalid)
	}
	out.NextDueAt = &next[0]
	out.Reason = recurringBatchReason(batch, cron.DispatchDue)
	return out, nil
}

func recurringBatchReason(batch cron.RecurringBatch, delivered cron.DispatchReason) cron.DispatchReason {
	switch {
	case len(batch.Due) > 0:
		return delivered
	case batch.Progress.Scan != nil:
		return cron.DispatchScanning
	case batch.Skipped.Count > 0:
		return cron.DispatchSkipped
	default:
		return cron.DispatchIdle
	}
}
