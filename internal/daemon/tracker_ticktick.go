package daemon

import (
	"context"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/tracker"
)

type trackerTickTickFetcher struct {
	runtime *TrackerRuntime
	legacy  tickticksync.Fetcher
}

func (f trackerTickTickFetcher) ForRun(ctx context.Context, c tickticksync.Config) (tickticksync.Session, error) {
	if _, named := tracker.RunSelection(ctx); !named {
		return f.legacy.ForRun(ctx, c)
	}
	connection, err := f.runtime.selectedMethodConnection(ctx, "ticktick", tracker.ActionRead)
	if err != nil {
		return nil, err
	}
	guard, err := f.runtime.sessionGuard(ctx, "ticktick")
	if err != nil {
		return nil, err
	}
	session, err := connection.clients.ticktick.ForRun(ctx, c)
	if err != nil {
		return nil, trackerClientError(err)
	}
	return &trackerTickTickSession{guard: guard, session: session}, nil
}

type trackerTickTickSession struct {
	guard   trackerSessionGuard
	session tickticksync.Session
}

func (s *trackerTickTickSession) Project(ctx context.Context) (tickticksync.Project, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return tickticksync.Project{}, err
	}
	return trackerCall(s.session.Project(ctx))
}
func (s *trackerTickTickSession) Data(ctx context.Context) (tickticksync.ProjectData, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return tickticksync.ProjectData{}, err
	}
	return trackerCall(s.session.Data(ctx))
}
func (s *trackerTickTickSession) Task(ctx context.Context, id string) (tickticksync.Task, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return tickticksync.Task{}, err
	}
	return trackerCall(s.session.Task(ctx, id))
}
func (s *trackerTickTickSession) ReadStatus(ctx context.Context, id string) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(tickticksync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.ReadStatus(ctx, id))
}
func (s *trackerTickTickSession) WriteStatus(ctx context.Context, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.writeContext(ctx, target)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(tickticksync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.WriteStatus(ctx, id, target, s.guard.admission(ctx, target, admit)))
}

var _ tickticksync.Fetcher = trackerTickTickFetcher{}
var _ tickticksync.Session = (*trackerTickTickSession)(nil)
var _ tickticksync.StatusSession = (*trackerTickTickSession)(nil)
