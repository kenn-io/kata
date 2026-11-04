package db

import (
	"context"
	"database/sql"
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
func (a CronSQL) ExplainRecurring(ctx context.Context, tx *sql.Tx, in DispatchRecurringCronIn) (out RecurringExplanation, err error) {
	defer func() {
		if err != nil {
			if out.Reason == "" {
				out.Reason = cron.DispatchError
			}
			out.NextDueAt = nil
		}
		out.Detail = out.Reason.PublicDetail()
	}()
	in.JobUID = strings.ToUpper(strings.TrimSpace(in.JobUID))
	g, err := a.recurringGateTx(ctx, tx, in, gateExplain)
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
	if len(next) > 0 {
		out.NextDueAt = &next[0]
	}
	switch {
	case len(batch.Due) > 0:
		out.Reason = cron.DispatchDue
	case batch.Progress.Scan != nil:
		out.Reason = cron.DispatchScanning
	case batch.Skipped.Count > 0:
		out.Reason = cron.DispatchSkipped
	default:
		out.Reason = cron.DispatchIdle
	}
	return out, nil
}
