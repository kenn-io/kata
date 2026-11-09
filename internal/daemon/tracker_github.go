package daemon

import (
	"context"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
	"time"
)

type trackerGitHubFetcher struct {
	runtime *TrackerRuntime
	legacy  githubsync.Fetcher
}

func (f trackerGitHubFetcher) ForBinding(ctx context.Context, b githubsync.Binding) (githubsync.Fetcher, error) {
	if _, named := tracker.RunSelection(ctx); !named {
		if scoped, ok := f.legacy.(githubsync.BindingSessionFetcher); ok {
			return scoped.ForBinding(ctx, b)
		}
		return f.legacy, nil
	}
	connection, err := f.runtime.selectedConnection(ctx, "github", tracker.ActionRead)
	if err != nil {
		return nil, err
	}
	guard, err := f.runtime.sessionGuard(ctx, "github")
	if err != nil {
		return nil, err
	}
	session := connection.clients.github
	if scoped, ok := session.(githubsync.BindingSessionFetcher); ok {
		session, err = scoped.ForBinding(ctx, b)
		if err != nil {
			return nil, trackerClientError(err)
		}
	}
	return &trackerGitHubSession{guard: guard, session: session}, nil
}
func (f trackerGitHubFetcher) Repository(ctx context.Context, host, owner, repo string) (githubsync.Repository, error) {
	s, err := f.ForBinding(ctx, githubsync.Binding{Host: host, Owner: owner, Repo: repo})
	if err != nil {
		return githubsync.Repository{}, err
	}
	return s.Repository(ctx, host, owner, repo)
}
func (f trackerGitHubFetcher) Issues(ctx context.Context, b githubsync.Binding, since *time.Time) ([]githubsync.Issue, error) {
	s, err := f.ForBinding(ctx, b)
	if err != nil {
		return nil, err
	}
	return s.Issues(ctx, b, since)
}
func (f trackerGitHubFetcher) Comments(ctx context.Context, b githubsync.Binding, number int) ([]githubsync.Comment, error) {
	s, err := f.ForBinding(ctx, b)
	if err != nil {
		return nil, err
	}
	return s.Comments(ctx, b, number)
}
func (f trackerGitHubFetcher) RepositoryComments(ctx context.Context, b githubsync.Binding, since *time.Time) (map[int][]githubsync.Comment, bool, error) {
	s, err := f.ForBinding(ctx, b)
	if err != nil {
		return nil, false, err
	}
	return s.RepositoryComments(ctx, b, since)
}
func (f trackerGitHubFetcher) ParentData(ctx context.Context, b githubsync.Binding, request githubsync.ParentRequest) (githubsync.ParentData, error) {
	s, err := f.ForBinding(ctx, b)
	if err != nil {
		return githubsync.ParentData{}, err
	}
	return s.ParentData(ctx, b, request)
}

type trackerGitHubSession struct {
	guard   trackerSessionGuard
	session githubsync.Fetcher
}

func (s *trackerGitHubSession) Repository(ctx context.Context, host, owner, repo string) (githubsync.Repository, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return githubsync.Repository{}, err
	}
	return trackerCall(s.session.Repository(ctx, host, owner, repo))
}
func (s *trackerGitHubSession) Issues(ctx context.Context, b githubsync.Binding, since *time.Time) ([]githubsync.Issue, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, err
	}
	return trackerCall(s.session.Issues(ctx, b, since))
}
func (s *trackerGitHubSession) Comments(ctx context.Context, b githubsync.Binding, number int) ([]githubsync.Comment, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, err
	}
	return trackerCall(s.session.Comments(ctx, b, number))
}
func (s *trackerGitHubSession) RepositoryComments(ctx context.Context, b githubsync.Binding, since *time.Time) (map[int][]githubsync.Comment, bool, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, false, err
	}
	return trackerCallWithCursor(s.session.RepositoryComments(ctx, b, since))
}
func (s *trackerGitHubSession) ParentData(ctx context.Context, b githubsync.Binding, request githubsync.ParentRequest) (githubsync.ParentData, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return githubsync.ParentData{}, err
	}
	return trackerCall(s.session.ParentData(ctx, b, request))
}
func (s *trackerGitHubSession) ReadStatus(ctx context.Context, c githubsync.Config, id string, number int) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(githubsync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.ReadStatus(ctx, c, id, number))
}
func (s *trackerGitHubSession) WriteStatus(ctx context.Context, c githubsync.Config, id string, number int, target string, admit func() error) (issuesync.StatusObservation, error) {
	ctx, err := s.guard.writeContext(ctx, target)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	session, ok := s.session.(githubsync.StatusSession)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	return trackerCall(session.WriteStatus(ctx, c, id, number, target, s.guard.admission(ctx, target, admit)))
}

func (s *trackerGitHubSession) StatusLocators(ctx context.Context, c githubsync.Config, page int) ([]githubsync.StatusLocator, int, error) {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil, 0, err
	}
	session, ok := s.session.(githubsync.StatusLocatorSession)
	if !ok {
		return nil, 0, tracker.ErrUnavailable
	}
	return trackerCallWithCursor(session.StatusLocators(ctx, c, page))
}
func (s *trackerGitHubSession) ReadStatuses(ctx context.Context, c githubsync.Config, targets []githubsync.StatusLocator) map[string]githubsync.StatusResult {
	ctx, err := s.guard.authorize(ctx)
	if err != nil {
		return nil
	}
	session, ok := s.session.(githubsync.StatusBatchSession)
	if !ok {
		return nil
	}
	return session.ReadStatuses(ctx, c, targets)
}

var _ githubsync.Fetcher = trackerGitHubFetcher{}
var _ githubsync.BindingSessionFetcher = trackerGitHubFetcher{}
var _ githubsync.Fetcher = (*trackerGitHubSession)(nil)
var _ githubsync.StatusSession = (*trackerGitHubSession)(nil)
var _ githubsync.StatusLocatorSession = (*trackerGitHubSession)(nil)
var _ githubsync.StatusBatchSession = (*trackerGitHubSession)(nil)
