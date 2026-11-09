package daemon

import (
	"context"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/twentysync"
)

type trackerTwentyFetcher struct {
	runtime *TrackerRuntime
	legacy  twentysync.Fetcher
}

func (f trackerTwentyFetcher) ForRun(ctx context.Context, c twentysync.Config) (twentysync.Session, error) {
	if _, named := tracker.RunSelection(ctx); !named {
		return f.legacy.ForRun(ctx, c)
	}
	connection, err := f.runtime.selectedMethodConnection(ctx, "twenty", tracker.ActionRead)
	if err != nil {
		return nil, err
	}
	guard, err := f.runtime.sessionGuard(ctx, "twenty")
	if err != nil {
		return nil, err
	}
	session, err := connection.clients.twenty.ForRun(ctx, c)
	if err != nil {
		return nil, trackerClientError(err)
	}
	return &trackerTwentySession{guard: guard, session: session}, nil
}

type trackerTwentySession struct {
	guard   trackerSessionGuard
	session twentysync.Session
}

func (s *trackerTwentySession) Workspace(ctx context.Context, c twentysync.Config) (twentysync.Workspace, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return twentysync.Workspace{}, err
	}
	return trackerCall(s.session.Workspace(ctx, c))
}
func (s *trackerTwentySession) Schema(ctx context.Context, c twentysync.Config) (twentysync.Schema, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return twentysync.Schema{}, err
	}
	return trackerCall(s.session.Schema(ctx, c))
}
func (s *trackerTwentySession) Tasks(ctx context.Context, c twentysync.Config) ([]twentysync.Task, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, err
	}
	return trackerCall(s.session.Tasks(ctx, c))
}
func (s *trackerTwentySession) ReadStatus(ctx context.Context, c twentysync.Config, id string) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(twentysync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.ReadStatus(ctx, c, id))
}
func (s *trackerTwentySession) WriteStatus(ctx context.Context, c twentysync.Config, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.writeContext(ctx, target)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(twentysync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.WriteStatus(ctx, c, id, target, s.guard.admission(ctx, target, admit)))
}

var _ twentysync.Fetcher = trackerTwentyFetcher{}
var _ twentysync.Session = (*trackerTwentySession)(nil)
var _ twentysync.StatusSession = (*trackerTwentySession)(nil)
