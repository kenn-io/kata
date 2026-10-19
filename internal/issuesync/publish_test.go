package issuesync

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type publishFake struct {
	prepare               func(context.Context, db.Issue)
	creates, reads, opens int
	lastRead              db.IssuePublicationRemote
	create                func(context.Context, func(context.Context) error) (db.IssuePublicationRemote, error)
	read                  func(context.Context, string) (db.IssuePublicationRemote, error)
}

func (f *publishFake) OpenPublish(context.Context, db.IssueSyncBinding) (PublishSession, error) {
	f.opens++
	return f, nil
}
func (f *publishFake) PrepareCreate(ctx context.Context, issue db.Issue) (jsontext.Value, error) {
	if f.prepare != nil {
		f.prepare(ctx, issue)
	}
	return []byte(`{"title":"Native"}`), nil
}
func (f *publishFake) Create(ctx context.Context, _ jsontext.Value, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
	f.creates++
	return f.create(ctx, admit)
}
func (f *publishFake) ReadCreated(ctx context.Context, identity db.IssuePublicationRemote) (db.IssuePublicationRemote, error) {
	f.reads++
	f.lastRead = identity
	id := identity.ExternalID
	if f.read != nil {
		return f.read(ctx, id)
	}
	return publishRemote(), nil
}
func publishRemote() db.IssuePublicationRemote {
	return db.IssuePublicationRemote{ExternalID: "page:remote", Status: "open", Version: time.Now().UTC()}
}

func TestPublishCreateAndMappedReplay(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, "example-project", "notion")
	i, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Author: "worker"})
	require.NoError(t, err)
	f := &publishFake{create: func(ctx context.Context, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
		require.NoError(t, admit(ctx))
		pending, e := s.PendingIssuePublication(ctx, b.ProjectID)
		require.NoError(t, e)
		require.Equal(t, "dispatching", pending.Phase)
		return publishRemote(), nil
	}}
	p := NewPublisher(PublisherConfig{Store: s, Adapter: f})
	req := PublishRequest{ProjectID: b.ProjectID, IssueID: i.ID, Provider: "notion", Actor: "worker"}
	result, err := p.Publish(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "created", result.Result)
	require.Equal(t, i.UID, result.IssueUID)
	result, err = p.Publish(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "already_published", result.Result)
	require.Equal(t, 1, f.creates)
	require.Equal(t, 1, f.opens)
	pending, err := s.PendingIssuePublication(t.Context(), b.ProjectID)
	require.NoError(t, err)
	require.Nil(t, pending)
}

func TestPublishAmbiguousNeverRetriesAndAdopts(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, "example-project", "notion")
	i, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Author: "worker"})
	require.NoError(t, err)
	now := time.Now().UTC()
	f := &publishFake{create: func(ctx context.Context, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
		require.NoError(t, admit(ctx))
		return db.IssuePublicationRemote{}, errors.New("secret provider diagnostics")
	}}
	p := NewPublisher(PublisherConfig{Store: s, Adapter: f, Clock: func() time.Time { return now }})
	req := PublishRequest{ProjectID: b.ProjectID, IssueID: i.ID, Provider: "notion", Actor: "worker"}
	_, err = p.Publish(t.Context(), req)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
	pending, err := s.PendingIssuePublication(t.Context(), b.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "dispatching", pending.Phase)
	req.ResolveNotCreated = true
	req.AttemptUID = pending.AttemptUID
	req.Message = "Inspected remote source"
	_, err = p.Publish(t.Context(), req)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	now = now.Add(time.Hour)
	req.ResolveNotCreated = false
	req.AttemptUID = ""
	req.Message = ""
	_, err = p.Publish(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, 1, f.creates)
	req.Adopt = "remote"
	result, err := p.Publish(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "recovered", result.Result)
	require.Equal(t, 1, f.creates)
}

func TestPublishRetainsPartialIdentityAfterCancellation(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, "example-project", "notion")
	i, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Author: "worker"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &publishFake{create: func(ctx context.Context, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
		require.NoError(t, admit(ctx))
		cancel()
		return db.IssuePublicationRemote{ExternalID: "page:remote", Locator: "7"}, context.Canceled
	}}
	p := NewPublisher(PublisherConfig{Store: s, Adapter: f})
	req := PublishRequest{ProjectID: b.ProjectID, IssueID: i.ID, Provider: "notion", Actor: "worker"}
	_, err = p.Publish(ctx, req)
	require.Error(t, err)
	pending, err := s.PendingIssuePublication(t.Context(), b.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "created", pending.Phase)
	require.Equal(t, "page:remote", pending.Remote.ExternalID)
	result, err := p.Publish(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "recovered", result.Result)
	require.Equal(t, "7", f.lastRead.Locator)
	require.Equal(t, 1, f.creates)
}

func TestPublishRecoveryAndDispatchFences(t *testing.T) {
	for _, scenario := range []string{"prepared", "disabled_before_send", "disabled_during_send", "horizon", "read_404", "restore", "wrong_scope", "resolve"} {
		t.Run(scenario, func(t *testing.T) {
			s := testStore(t)
			b := testBinding(t, s, "example-project", "notion")
			i, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Author: "worker"})
			require.NoError(t, err)
			now := time.Now().UTC()
			dispatched := 0
			delivered := 0
			disable := func() {
				_, err := s.DisableIssueSyncBinding(t.Context(), b.ProjectID)
				require.NoError(t, err)
			}
			f := &publishFake{}
			f.create = func(ctx context.Context, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
				if scenario == "prepared" {
					return db.IssuePublicationRemote{}, errors.New("pre-dispatch")
				}
				if scenario == "disabled_before_send" {
					disable()
				}
				if scenario == "horizon" {
					now = now.Add(26 * time.Minute)
				}
				if err := admit(ctx); err != nil {
					return db.IssuePublicationRemote{}, err
				}
				dispatched++
				if scenario == "disabled_during_send" {
					disable()
				}
				if scenario == "resolve" {
					return db.IssuePublicationRemote{}, errors.New("creation response was lost")
				}
				if scenario == "read_404" {
					return publishRemote(), errors.New("followup failed")
				}
				return publishRemote(), nil
			}
			p := NewPublisher(PublisherConfig{Store: s, Adapter: f, Clock: func() time.Time { return now }, EventSink: func(_ context.Context, _ int64, ev []db.Event) error { delivered += len(ev); return nil }})
			req := PublishRequest{ProjectID: b.ProjectID, IssueID: i.ID, Provider: "notion", Actor: "worker"}
			if scenario == "restore" || scenario == "wrong_scope" {
				req.Adopt = "remote"
				_, err = p.Publish(t.Context(), req)
				require.Error(t, err)
				require.Zero(t, f.opens)
				disable()
				if scenario == "wrong_scope" {
					f.read = func(context.Context, string) (db.IssuePublicationRemote, error) {
						return db.IssuePublicationRemote{}, errors.New("wrong source secret")
					}
				}
			}
			result, err := p.Publish(t.Context(), req)
			if scenario == "restore" {
				require.NoError(t, err)
				require.Equal(t, "recovered", result.Result)
				require.Zero(t, f.creates)
				return
			}
			require.Error(t, err)
			pending, err := s.PendingIssuePublication(t.Context(), b.ProjectID)
			require.NoError(t, err)
			if scenario == "wrong_scope" {
				require.Nil(t, pending)
				return
			}
			require.NotNil(t, pending)
			if scenario == "prepared" || scenario == "disabled_before_send" || scenario == "horizon" {
				require.Zero(t, dispatched)
				require.Equal(t, "prepared", pending.Phase)
			}
			if scenario == "disabled_during_send" {
				require.Equal(t, "created", pending.Phase)
			}
			if scenario == "read_404" {
				f.read = func(context.Context, string) (db.IssuePublicationRemote, error) {
					return db.IssuePublicationRemote{}, errors.New("404 secret")
				}
			}
			if scenario == "resolve" {
				now = now.Add(time.Hour)
				req.ResolveNotCreated = true
				req.AttemptUID = "wrong"
				req.Message = "Inspected source"
				_, err = p.Publish(t.Context(), req)
				require.Error(t, err)
				req.AttemptUID = pending.AttemptUID
				result, err = p.Publish(t.Context(), req)
				require.NoError(t, err)
				require.Equal(t, "resolved_not_created", result.Result)
				require.Equal(t, 1, delivered)
				saved, e := s.PendingIssuePublication(t.Context(), b.ProjectID)
				require.NoError(t, e)
				require.Nil(t, saved)
				require.Equal(t, 1, f.creates)
				return
			}
			now = now.Add(time.Hour)
			_, err = p.Publish(t.Context(), req)
			if scenario == "disabled_during_send" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, 1, f.creates)
		})
	}
}

func TestPublishInvalidRecoveryNeverOpensProvider(t *testing.T) {
	for _, req := range []PublishRequest{
		{Adopt: "remote", ResolveNotCreated: true, AttemptUID: "attempt", Message: "inspected"},
		{ResolveNotCreated: true, AttemptUID: "attempt", Message: "  "},
		{ResolveNotCreated: true, Message: "inspected"},
		{AttemptUID: "attempt"},
	} {
		s := testStore(t)
		b := testBinding(t, s, "example-project", "notion")
		i, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Author: "worker"})
		require.NoError(t, err)
		req.ProjectID = b.ProjectID
		req.IssueID = i.ID
		req.Provider = "notion"
		req.Actor = "worker"
		f := &publishFake{}
		_, err = NewPublisher(PublisherConfig{Store: s, Adapter: f}).Publish(t.Context(), req)
		failure, ok := errors.AsType[*PublishError](err)
		require.True(t, ok, "invalid recovery must return a typed publication error")
		require.Equal(t, "invalid_publish_request", failure.Code)
		require.Zero(t, f.opens)
	}
}

func TestPublishConcurrentRecoveryCannotClearLiveRequest(t *testing.T) {
	s := testStore(t)
	b := testBinding(t, s, "example-project", "notion")
	i, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Author: "worker"})
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	f := &publishFake{create: func(ctx context.Context, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
		if err := admit(ctx); err != nil {
			return db.IssuePublicationRemote{}, err
		}
		close(entered)
		<-release
		return publishRemote(), nil
	}}
	p := NewPublisher(PublisherConfig{Store: s, Adapter: f})
	req := PublishRequest{ProjectID: b.ProjectID, IssueID: i.ID, Provider: "notion", Actor: "worker"}
	done := make(chan error, 1)
	go func() { _, err := p.Publish(t.Context(), req); done <- err }()
	receive(t, entered)
	pending, err := s.PendingIssuePublication(t.Context(), b.ProjectID)
	require.NoError(t, err)
	_, err = p.Publish(t.Context(), req)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	recovery := req
	recovery.ResolveNotCreated = true
	recovery.AttemptUID = pending.AttemptUID
	recovery.Message = "Inspected source"
	_, err = p.Publish(t.Context(), recovery)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	close(release)
	require.NoError(t, receive(t, done))
	require.Equal(t, 1, f.creates)
}

func TestPublishRecoveryDoesNotRestoreIntentDiscardedByModeChange(t *testing.T) {
	store := testStore(t)
	binding := testBinding(t, store, "example-project", "notion")
	setMode := func(mode string) {
		var err error
		binding, err = store.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{
			ProjectID: binding.ProjectID, Provider: binding.Provider, SourceKey: binding.SourceKey,
			RemoteID: binding.RemoteID, DisplayName: binding.DisplayName, IntervalSeconds: 60,
			Config: []byte(`{"status_sync":"` + mode + `"}`),
		})
		require.NoError(t, err)
	}
	setMode("two-way")
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: binding.ProjectID, Title: "Native", Author: "worker"})
	require.NoError(t, err)
	provider := &publishFake{create: func(ctx context.Context, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
		require.NoError(t, admit(ctx))
		_, _, _, err := store.CloseIssue(ctx, issue.ID, "done", "worker", "Closed during publication", nil)
		require.NoError(t, err)
		setMode("one-way")
		_, _, _, err = store.ReopenIssue(ctx, issue.ID, "worker")
		require.NoError(t, err)
		setMode("two-way")
		return publishRemote(), nil
	}}
	publisher := NewPublisher(PublisherConfig{Store: store, Adapter: provider})
	request := PublishRequest{ProjectID: binding.ProjectID, IssueID: issue.ID, Provider: "notion", Actor: "worker"}
	_, err = publisher.Publish(t.Context(), request)
	require.Error(t, err, "configuration changes fence the original attachment")
	request.Adopt = "remote"
	result, err := publisher.Publish(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "recovered", result.Result)
	require.Equal(t, 1, provider.creates)
	require.Equal(t, 1, provider.reads)
	mapping, err := store.ImportMappingBySource(t.Context(), binding.ProjectID, binding.SourceKey, "issue", "page:remote")
	require.NoError(t, err)
	current, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, "open", current.Status)
	require.Nil(t, privateMapping(t, store, db.IssueStatusMapping{Mapping: mapping}).PendingEventUID,
		"adoption must not restore the discarded close event")
}

func TestPublishRejectsContentEditedDuringPreflight(t *testing.T) {
	for _, field := range []string{"title", "body"} {
		t.Run(field, func(t *testing.T) {
			s := testStore(t)
			b := testBinding(t, s, "example-project", "notion")
			i, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Author: "worker"})
			require.NoError(t, err)
			f := &publishFake{prepare: func(ctx context.Context, issue db.Issue) {
				edit := db.EditIssueParams{IssueID: issue.ID, Actor: "worker"}
				if field == "title" {
					edit.Title = new("New title")
				} else {
					edit.Body = new("New body")
				}
				_, _, _, err := s.EditIssue(ctx, edit)
				require.NoError(t, err)
			}, create: func(context.Context, func(context.Context) error) (db.IssuePublicationRemote, error) {
				return publishRemote(), nil
			}}
			_, err = NewPublisher(PublisherConfig{Store: s, Adapter: f}).Publish(t.Context(), PublishRequest{ProjectID: b.ProjectID, IssueID: i.ID, Provider: "notion", Actor: "worker"})
			require.ErrorIs(t, err, db.ErrImportValidation)
			require.Zero(t, f.creates)
			pending, err := s.PendingIssuePublication(t.Context(), b.ProjectID)
			require.NoError(t, err)
			require.Nil(t, pending)
			status, err := s.IssueSyncStatusByProject(t.Context(), b.ProjectID)
			require.NoError(t, err)
			require.Nil(t, status.SyncStartedAt)
		})
	}
}
