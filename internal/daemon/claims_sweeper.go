package daemon

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
)

const (
	defaultTimedClaimSweepInterval = 30 * time.Second
	defaultTimedClaimSweepLimit    = 100
)

// TimedClaimSweeper expires authoritative hub timed claims and fans emitted
// claim.expired events out through the daemon's normal event surfaces.
type TimedClaimSweeper struct {
	DB          db.Storage
	Broadcaster *EventBroadcaster
	Hooks       hooks.Sink
	Interval    time.Duration
	Limit       int
	OnError     func(error)
}

// NewTimedClaimSweeper creates a timed-claim sweeper with default event sinks.
func NewTimedClaimSweeper(store db.Storage, broadcaster *EventBroadcaster, sink hooks.Sink) *TimedClaimSweeper {
	if broadcaster == nil {
		broadcaster = NewEventBroadcaster()
	}
	if sink == nil {
		sink = hooks.NewNoop()
	}
	return &TimedClaimSweeper{DB: store, Broadcaster: broadcaster, Hooks: sink}
}

// RunOnce expires timed claims for all enabled hub bindings once.
func (s *TimedClaimSweeper) RunOnce(ctx context.Context, now time.Time) error {
	bindings, err := s.DB.ListFederationBindings(ctx)
	if err != nil {
		return err
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
		for _, event := range events {
			s.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &event, ProjectID: event.ProjectID})
			s.Hooks.Enqueue(event)
		}
	}
	return errors.Join(errs...)
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
		if err := s.RunOnce(ctx, time.Now().UTC()); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			if s.OnError != nil {
				s.OnError(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
