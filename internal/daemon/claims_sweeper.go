package daemon

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
)

const (
	defaultTimedClaimSweepInterval = 30 * time.Second
	defaultTimedClaimSweepLimit    = 100
)

// TimedClaimSweeper expires authoritative hub timed claims and fans emitted
// claim.expired events out through the daemon's normal event surfaces.
type TimedClaimSweeper struct {
	DB            db.Storage
	Publish       EventPublisher
	Interval      time.Duration
	Limit         int
	OnError       func(error)
	IdleAdmission activity.WaitableAdmission
}

// NewTimedClaimSweeper creates a timed-claim sweeper that fans expiry events
// out through publisher.
func NewTimedClaimSweeper(store db.Storage, publisher EventPublisher) *TimedClaimSweeper {
	return &TimedClaimSweeper{DB: store, Publish: publisher}
}

// RunOnce expires timed claims for all enabled hub bindings once.
func (s *TimedClaimSweeper) RunOnce(ctx context.Context, now time.Time) error {
	_, _, err := s.runOnce(ctx, now)
	return err
}

func (s *TimedClaimSweeper) runOnce(ctx context.Context, now time.Time) (<-chan struct{}, bool, error) {
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
	bindings, err := s.DB.ListFederationBindings(ctx)
	if err != nil {
		return nil, false, err
	}
	limit := s.Limit
	if limit <= 0 {
		limit = defaultTimedClaimSweepLimit
	}
	var errs []error
	for _, binding := range bindings {
		if !binding.Enabled || binding.Role != db.FederationRoleHub {
			continue
		}
		project, err := s.DB.ProjectByID(ctx, binding.ProjectID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if project.DeletedAt != nil {
			continue
		}
		events, err := s.DB.ExpireTimedClaimsForProject(ctx, binding.ProjectID, now, limit)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.Publish.EventsFrom(binding.ProjectID, events, fork)
	}
	return nil, false, errors.Join(errs...)
}

// Run expires timed claims on a ticker until the context is canceled.
func (s *TimedClaimSweeper) Run(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultTimedClaimSweepInterval
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
