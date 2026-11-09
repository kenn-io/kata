package daemon

import (
	"context"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/tracker"
)

type trackerLinearFetcher struct {
	runtime *TrackerRuntime
	legacy  linearsync.Fetcher
}

func (f trackerLinearFetcher) ForRun(ctx context.Context, c linearsync.Config) (linearsync.Session, error) {
	if _, named := tracker.RunSelection(ctx); !named {
		return f.legacy.ForRun(ctx, c)
	}
	connection, err := f.runtime.selectedMethodConnection(ctx, "linear", tracker.ActionRead)
	if err != nil {
		return nil, err
	}
	guard, err := f.runtime.sessionGuard(ctx, "linear")
	if err != nil {
		return nil, err
	}
	session, err := connection.clients.linear.ForRun(ctx, c)
	if err != nil {
		return nil, trackerClientError(err)
	}
	return &trackerLinearSession{guard: guard, session: session}, nil
}

type trackerLinearSession struct {
	guard   trackerSessionGuard
	session linearsync.Session
}

func (s *trackerLinearSession) Scope(ctx context.Context, c linearsync.Config) (linearsync.Scope, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return linearsync.Scope{}, err
	}
	return trackerCall(s.session.Scope(ctx, c))
}
func (s *trackerLinearSession) States(ctx context.Context, c linearsync.Config) ([]linearsync.State, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, err
	}
	return trackerCall(s.session.States(ctx, c))
}
func (s *trackerLinearSession) Issues(ctx context.Context, c linearsync.Config) ([]linearsync.Issue, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, err
	}
	return trackerCall(s.session.Issues(ctx, c))
}
func (s *trackerLinearSession) ReadStatus(ctx context.Context, c linearsync.Config, id string) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(linearsync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.ReadStatus(ctx, c, id))
}
func (s *trackerLinearSession) WriteStatus(ctx context.Context, c linearsync.Config, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.writeContext(ctx, target)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(linearsync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.WriteStatus(ctx, c, id, target, s.guard.admission(ctx, target, admit)))
}

var _ linearsync.Fetcher = trackerLinearFetcher{}
var _ linearsync.Session = (*trackerLinearSession)(nil)
var _ linearsync.StatusSession = (*trackerLinearSession)(nil)
