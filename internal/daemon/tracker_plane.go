package daemon

import (
	"context"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tracker"
)

type trackerPlaneFetcher struct {
	runtime *TrackerRuntime
	legacy  planesync.Fetcher
}

func (f trackerPlaneFetcher) ForRun(ctx context.Context, c planesync.Config) (planesync.Session, error) {
	if _, named := tracker.RunSelection(ctx); !named {
		return f.legacy.ForRun(ctx, c)
	}
	connection, err := f.runtime.selectedConnection(ctx, "plane", tracker.ActionRead)
	if err != nil {
		return nil, err
	}
	guard, err := f.runtime.sessionGuard(ctx, "plane")
	if err != nil {
		return nil, err
	}
	session, err := connection.clients.plane.ForRun(ctx, c)
	if err != nil {
		return nil, trackerClientError(err)
	}
	return &trackerPlaneSession{guard: guard, session: session}, nil
}

type trackerPlaneSession struct {
	guard   trackerSessionGuard
	session planesync.Session
}

func (s *trackerPlaneSession) Project(ctx context.Context, c planesync.Config) (planesync.Project, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return planesync.Project{}, err
	}
	return trackerCall(s.session.Project(ctx, c))
}
func (s *trackerPlaneSession) States(ctx context.Context, c planesync.Config) ([]planesync.State, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, err
	}
	return trackerCall(s.session.States(ctx, c))
}
func (s *trackerPlaneSession) WorkItems(ctx context.Context, c planesync.Config) ([]planesync.WorkItem, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, err
	}
	return trackerCall(s.session.WorkItems(ctx, c))
}
func (s *trackerPlaneSession) ReadStatus(ctx context.Context, c planesync.Config, id string) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(planesync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.ReadStatus(ctx, c, id))
}
func (s *trackerPlaneSession) WriteStatus(ctx context.Context, c planesync.Config, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.writeContext(ctx, target)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(planesync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.WriteStatus(ctx, c, id, target, s.guard.admission(ctx, target, admit)))
}

var _ planesync.Fetcher = trackerPlaneFetcher{}
var _ planesync.Session = (*trackerPlaneSession)(nil)
var _ planesync.StatusSession = (*trackerPlaneSession)(nil)
