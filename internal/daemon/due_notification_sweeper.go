package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
)

const defaultDueNotificationSweepInterval = 30 * time.Second

// maxRecurringBatchesPerSweep bounds one job's work per sweep so a huge
// backlog cannot starve other jobs; the next sweep continues from progress.
const maxRecurringBatchesPerSweep = 20

// DueNotificationSweeper surfaces reached planning dates and native recurring
// child issues through the same notify.* inbox slots written by kata notify,
// and records each newly reached scheduled_on value as issue.scheduled_reached.
type DueNotificationSweeper struct {
	CronRecorder      *CronDispatchRecorder
	DB                db.Storage
	Publish           EventPublisher
	DefaultTimezone   string
	Interval          time.Duration
	OnError           func(error)
	IdleAdmission     activity.WaitableAdmission
	recurringAfterUID string
	// reachedFloor caches the database's stored reached-schedule floor.
	reachedFloor *time.Time
}

// NewDueNotificationSweeper constructs a sweeper for the provided store and event publisher.
func NewDueNotificationSweeper(store db.Storage, publisher EventPublisher, defaultTimezone string) *DueNotificationSweeper {
	return &DueNotificationSweeper{DB: store, Publish: publisher, DefaultTimezone: defaultTimezone}
}

// RunOnce reconciles every current candidate once. One malformed issue is
// reported without preventing other due issues from reaching their inboxes.
func (s *DueNotificationSweeper) RunOnce(ctx context.Context, now time.Time) error {
	_, _, err, _ := s.runOnce(ctx, now)
	return err
}

func (s *DueNotificationSweeper) runOnce(ctx context.Context, now time.Time) (<-chan struct{}, bool, error, error) {
	var idleLease *activity.Lease
	if s.IdleAdmission != nil {
		var admitted bool
		var retry <-chan struct{}
		idleLease, admitted, retry = s.IdleAdmission()
		if !admitted {
			return retry, true, nil, nil
		}
		defer idleLease.Release()
	}
	var fork activity.Admission
	if idleLease != nil {
		fork = idleLease.Fork
	}
	var errs []error
	if s.reachedFloor == nil {
		// Without a floor the sweep still delivers inbox requests but records
		// no reached schedule, so a failed read cannot replay overdue dates.
		if floor, err := s.DB.EnsureScheduledReachedFloor(ctx, now); err != nil {
			errs = append(errs, err)
		} else {
			s.reachedFloor = &floor
		}
	}
	ids, err := s.DB.ListDueNotificationIssueIDs(ctx, now)
	if err != nil {
		errs = append(errs, err)
	}
	for _, issueID := range ids {
		result, err := s.DB.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
			IssueID: issueID, Now: now, DefaultTimezone: s.DefaultTimezone, ScheduledReachedFloor: s.reachedFloor,
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, event := range []*db.Event{result.Reached, result.Event} {
			if event != nil {
				s.Publish.EventFrom(event.ProjectID, *event, fork)
			}
		}
		if result.NotificationErr != nil {
			errs = append(errs, fmt.Errorf("plan inbox request for issue %d: %w", issueID, result.NotificationErr))
		}
	}
	alerts := append([]error(nil), errs...)
	started := time.Now()
	candidates, err := s.DB.ListRecurringCronJobs(ctx, s.recurringAfterUID, 100)
	if err != nil {
		errs = append(errs, err)
		alerts = append(alerts, err)
		s.recurringAfterUID = ""
	} else {
		for _, candidate := range candidates {
			s.recurringAfterUID = candidate.JobUID
			dispatchErr, alert := s.dispatchRecurring(ctx, candidate, now, fork)
			errs = append(errs, dispatchErr)
			alerts = append(alerts, alert)
		}
		if len(candidates) < 100 {
			s.recurringAfterUID = ""
		}
	}
	if s.CronRecorder != nil {
		s.CronRecorder.FinishCycle(now, time.Since(started), err == nil && len(candidates) < 100, err != nil)
	}
	return nil, false, errors.Join(errs...), errors.Join(alerts...)
}

// dispatchRecurring drains a job's backlog in bounded batches so a long
// pause catches up within one sweep instead of one batch per interval.
func (s *DueNotificationSweeper) dispatchRecurring(ctx context.Context, candidate db.RecurringCronCandidate, now time.Time, fork activity.Admission) (error, error) {
	in := db.DispatchRecurringCronIn{ProjectID: candidate.ProjectID, JobUID: candidate.JobUID, ExpectedEventUID: candidate.ExpectedEventUID, Now: now}
	for range maxRecurringBatchesPerSweep {
		out, err := s.DB.DispatchRecurringCron(ctx, in)
		// Publish committed events before best-effort diagnostic I/O.
		for _, event := range out.Events {
			s.Publish.EventFrom(event.ProjectID, event, fork)
		}
		changed := true
		if s.CronRecorder != nil {
			// Log failures remain visible in health without repeating sweep alerts.
			changed, _ = s.CronRecorder.Record(ctx, candidate, out, err, now)
		}
		if err != nil {
			err = fmt.Errorf("dispatch recurring job %s in project %d: %w", candidate.JobUID, candidate.ProjectID, err)
			var alert error
			if changed {
				alert = err
			}
			return err, alert
		}
		if !out.More {
			break
		}
	}
	return nil, nil
}

// Run performs one startup pass, then reconciles every interval until the
// daemon context is canceled.
func (s *DueNotificationSweeper) Run(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultDueNotificationSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		retry, denied, err, alert := s.runOnce(ctx, time.Now().UTC())
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			if s.OnError != nil && alert != nil {
				s.OnError(alert)
			}
		}
		if denied && retry != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-retry:
				continue
			}
		}
		if denied {
			<-ctx.Done()
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
