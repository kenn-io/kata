package daemon

import (
	"context"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/tracker"
	"time"
)

type trackerNotionFetcher struct {
	runtime *TrackerRuntime
	legacy  notionsync.Fetcher
}

func (f trackerNotionFetcher) ForRun(ctx context.Context) (notionsync.Session, error) {
	if _, named := tracker.RunSelection(ctx); !named {
		return f.legacy.ForRun(ctx)
	}
	connection, err := f.runtime.selectedConnection(ctx, "notion", tracker.ActionRead)
	if err != nil {
		return nil, err
	}
	guard, err := f.runtime.sessionGuard(ctx, "notion")
	if err != nil {
		return nil, err
	}
	session, err := connection.clients.notion.ForRun(ctx)
	if err != nil {
		return nil, trackerClientError(err)
	}
	return &trackerNotionSession{guard: guard, session: session}, nil
}

type trackerNotionSession struct {
	guard   trackerSessionGuard
	session notionsync.Session
}

func (s *trackerNotionSession) Database(ctx context.Context, id string) (notionsync.Database, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return notionsync.Database{}, err
	}
	return trackerCall(s.session.Database(ctx, id))
}
func (s *trackerNotionSession) DataSource(ctx context.Context, id string) (notionsync.DataSource, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return notionsync.DataSource{}, err
	}
	return trackerCall(s.session.DataSource(ctx, id))
}
func (s *trackerNotionSession) Pages(ctx context.Context, c notionsync.Config, since *time.Time) ([]notionsync.Page, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, err
	}
	return trackerCall(s.session.Pages(ctx, c, since))
}
func (s *trackerNotionSession) Content(ctx context.Context, c notionsync.Config, p notionsync.Page) (notionsync.PageContent, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return notionsync.PageContent{}, err
	}
	return trackerCall(s.session.Content(ctx, c, p))
}
func (s *trackerNotionSession) ReadStatus(ctx context.Context, c notionsync.Config, id string) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(notionsync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.ReadStatus(ctx, c, id))
}
func (s *trackerNotionSession) WriteStatus(ctx context.Context, c notionsync.Config, id, target string, admit func() error) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.writeContext(ctx, target)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(notionsync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.WriteStatus(ctx, c, id, target, s.guard.admission(ctx, target, admit)))
}

var _ notionsync.Fetcher = trackerNotionFetcher{}
var _ notionsync.Session = (*trackerNotionSession)(nil)
var _ notionsync.StatusSession = (*trackerNotionSession)(nil)
