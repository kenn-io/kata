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
	DB                db.Storage
	Publish           EventPublisher
	DefaultTimezone   string
	Interval          time.Duration
	OnError           func(error)
	IdleAdmission     activity.WaitableAdmission
	CronRecorder      *CronDispatchRecorder
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
	_, _, err := s.runOnce(ctx, now)
	return err
}

func (s *DueNotificationSweeper) runOnce(ctx context.Context, now time.Time) (<-chan struct{}, bool, error) {
	var idleLease *activity.Lease
	if s.IdleAdmission != nil {
		var admitted bool
		var retry <-chan struct{}
		idleLease, admitted, retry = s.IdleAdmission()
		if !admitted {
			return retry, true, nil
		}
		defer idleLease.Release()
	}
	var fork activity.Admission
	if idleLease != nil {
		fork = idleLease.Fork
	}
	var errs []error
	started := time.Now()
	if s.reachedFloor == nil {
		// Without a floor the sweep still delivers inbox requests but records
		// no reached schedule, so a failed read cannot replay overdue dates.
		if floor, err := s.DB.EnsureScheduledReachedFloor(ctx, now); err != nil {
			errs = append(errs, err)
		} else {
			s.reachedFloor = &floor
		}
	}
	ids, err := s.DB.ListDueNotificationIssueIDs(ctx)
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
	candidates, err := s.DB.ListRecurringCronJobs(ctx, s.recurringAfterUID, 100)
	if err != nil {
		errs = append(errs, err)
		// A failed page cannot establish a complete observation cycle.
		s.recurringAfterUID = ""
	} else {
		for _, candidate := range candidates {
			s.recurringAfterUID = candidate.JobUID
			if err := s.dispatchRecurring(ctx, candidate, now, fork); err != nil {
				errs = append(errs, err)
			}
		}
		if len(candidates) < 100 {
			s.recurringAfterUID = ""
		}
	}
	if s.CronRecorder != nil {
		s.CronRecorder.FinishSweep(now, time.Since(started), err == nil && len(candidates) < 100, err != nil)
	}
	return nil, false, errors.Join(errs...)
}

// dispatchRecurring drains a job's backlog in bounded batches so a long
// pause catches up within one sweep instead of one batch per interval.
func (s *DueNotificationSweeper) dispatchRecurring(ctx context.Context, candidate db.RecurringCronCandidate, now time.Time, fork activity.Admission) error {
	in := db.DispatchRecurringCronIn{ProjectID: candidate.ProjectID, JobUID: candidate.JobUID, ExpectedEventUID: candidate.ExpectedEventUID, Now: now}
	for range maxRecurringBatchesPerSweep {
		out, err := s.DB.DispatchRecurringCron(ctx, in)
		if err != nil {
			report := true
			if s.CronRecorder != nil {
				var logErr error
				report, logErr = s.CronRecorder.Record(ctx, candidate, out, err, now)
				err = errors.Join(err, logErr)
			}
			return recurringSweepError{err: fmt.Errorf("dispatch recurring job %s in project %d: %w", candidate.JobUID, candidate.ProjectID, err), report: report}
		}
		for _, event := range out.Events {
			s.Publish.EventFrom(event.ProjectID, event, fork)
		}
		if s.CronRecorder != nil {
			if changed, logErr := s.CronRecorder.Record(ctx, candidate, out, nil, now); logErr != nil && changed && s.OnError != nil {
				s.OnError(fmt.Errorf("cron dispatch log: %w", logErr))
			}
		}
		if !out.More {
			return nil
		}
	}
	return nil
}

type recurringSweepError struct {
	err    error
	report bool
}

func (e recurringSweepError) Error() string { return e.err.Error() }
func (e recurringSweepError) Unwrap() error { return e.err }

// recurringAlerts keeps RunOnce's full error evidence while suppressing only
// unchanged recurring-job alerts. Other notification and listing errors repeat.
func recurringAlerts(err error) error {
	if e, ok := err.(recurringSweepError); ok {
		if !e.report {
			return nil
		}
		return e.err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var alerts []error
		for _, child := range joined.Unwrap() {
			alerts = append(alerts, recurringAlerts(child))
		}
		return errors.Join(alerts...)
	}
	return err
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
		retry, denied, err := s.runOnce(ctx, time.Now().UTC())
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			if alert := recurringAlerts(err); alert != nil && s.OnError != nil {
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
