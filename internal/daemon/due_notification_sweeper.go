package daemon

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
)

const defaultDueNotificationSweepInterval = 30 * time.Second

// DueNotificationSweeper surfaces reached scheduled and deadline dates in the
// same notify.* inbox slots written by kata notify.
type DueNotificationSweeper struct {
	DB              db.Storage
	Publish         EventPublisher
	DefaultTimezone string
	Interval        time.Duration
	OnError         func(error)
	IdleAdmission   activity.WaitableAdmission
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
	ids, err := s.DB.ListDueNotificationIssueIDs(ctx)
	if err != nil {
		return nil, false, err
	}
	var errs []error
	for _, issueID := range ids {
		result, err := s.DB.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
			IssueID: issueID, Now: now, DefaultTimezone: s.DefaultTimezone,
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if result.Changed && result.Event != nil {
			s.Publish.EventFrom(result.Event.ProjectID, *result.Event, fork)
		}
	}
	return nil, false, errors.Join(errs...)
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
			if s.OnError != nil {
				s.OnError(err)
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
