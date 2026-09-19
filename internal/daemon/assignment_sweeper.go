package daemon

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
)

const (
	defaultAssignmentSweepInterval = 30 * time.Second
	defaultAssignmentSweepLimit    = 100
)

// AssignmentSweeper expires due issue assignments for local and hub projects.
type AssignmentSweeper struct {
	DB            db.Storage
	Publish       EventPublisher
	Interval      time.Duration
	Limit         int
	OnError       func(error)
	IdleAdmission activity.WaitableAdmission
}

// NewAssignmentSweeper creates an assignment sweeper that publishes committed expiry events.
func NewAssignmentSweeper(store db.Storage, publisher EventPublisher) *AssignmentSweeper {
	return &AssignmentSweeper{DB: store, Publish: publisher}
}

// RunOnce performs one bounded pass over every active authoritative project.
func (s *AssignmentSweeper) RunOnce(ctx context.Context, now time.Time) error {
	_, _, err := s.runOnce(ctx, now)
	return err
}

func (s *AssignmentSweeper) runOnce(ctx context.Context, now time.Time) (<-chan struct{}, bool, error) {
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
	projects, err := s.DB.ListProjects(ctx)
	if err != nil {
		return nil, false, err
	}
	bindings, err := s.DB.ListFederationBindings(ctx)
	if err != nil {
		return nil, false, err
	}
	spokes := make(map[int64]bool, len(bindings))
	for _, binding := range bindings {
		if binding.Enabled && binding.Role == db.FederationRoleSpoke {
			spokes[binding.ProjectID] = true
		}
	}
	limit := s.Limit
	if limit <= 0 {
		limit = defaultAssignmentSweepLimit
	}
	var errs []error
	for _, project := range projects {
		if spokes[project.ID] {
			continue
		}
		events, err := s.DB.ExpireAssignments(ctx, db.ExpireAssignmentsParams{
			ProjectID: project.ID,
			Now:       now,
			Limit:     limit,
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.Publish.EventsFrom(project.ID, events, fork)
	}
	return nil, false, errors.Join(errs...)
}

// Run expires assignments on a ticker until the context is canceled.
func (s *AssignmentSweeper) Run(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultAssignmentSweepInterval
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
