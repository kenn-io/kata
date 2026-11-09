package daemon

import (
	"context"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/tracker"
	"time"
)

type trackerTodoistFetcher struct {
	runtime *TrackerRuntime
	legacy  todoistsync.Fetcher
}

func (f trackerTodoistFetcher) Account(ctx context.Context) (string, error) {
	if _, named := tracker.RunSelection(ctx); !named {
		return f.legacy.Account(ctx)
	}
	connection, err := f.runtime.selectedMethodConnection(ctx, "todoist", tracker.ActionRead)
	if err != nil {
		return "", err
	}
	return trackerCall(connection.clients.todoist.Account(ctx))
}
func (f trackerTodoistFetcher) ForRun(ctx context.Context, c todoistsync.Config) (todoistsync.Session, error) {
	if _, named := tracker.RunSelection(ctx); !named {
		return f.legacy.ForRun(ctx, c)
	}
	connection, err := f.runtime.selectedMethodConnection(ctx, "todoist", tracker.ActionRead)
	if err != nil {
		return nil, err
	}
	guard, err := f.runtime.sessionGuard(ctx, "todoist")
	if err != nil {
		return nil, err
	}
	session, err := connection.clients.todoist.ForRun(ctx, c)
	if err != nil {
		return nil, trackerClientError(err)
	}
	return &trackerTodoistSession{guard: guard, session: session}, nil
}

type trackerTodoistSession struct {
	guard   trackerSessionGuard
	session todoistsync.Session
}

func (s *trackerTodoistSession) Project(ctx context.Context, c todoistsync.Config) (todoistsync.Project, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return todoistsync.Project{}, err
	}
	return trackerCall(s.session.Project(ctx, c))
}
func (s *trackerTodoistSession) Tasks(ctx context.Context, c todoistsync.Config, since, until time.Time) ([]todoistsync.Task, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, err
	}
	return trackerCall(s.session.Tasks(ctx, c, since, until))
}
func (s *trackerTodoistSession) ReadStatus(ctx context.Context, c todoistsync.Config, target todoistsync.StatusTarget) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(todoistsync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.ReadStatus(ctx, c, target))
}
func (s *trackerTodoistSession) WriteStatus(ctx context.Context, c todoistsync.Config, target todoistsync.StatusTarget, desired string, admit func() error) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.writeContext(ctx, desired)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(todoistsync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.WriteStatus(ctx, c, target, desired, s.guard.admission(ctx, desired, admit)))
}

var _ todoistsync.Fetcher = trackerTodoistFetcher{}
var _ todoistsync.Session = (*trackerTodoistSession)(nil)
var _ todoistsync.StatusSession = (*trackerTodoistSession)(nil)
