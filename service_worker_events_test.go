package kata

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
)

type recordingWorkerSink struct {
	mu     sync.Mutex
	events []db.Event
}

func (s *recordingWorkerSink) Enqueue(evt db.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evt)
}

func (s *recordingWorkerSink) ids() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.events))
	for _, evt := range s.events {
		out = append(out, evt.ID)
	}
	return out
}

type workerEventFetcher struct {
	issues []githubsync.Issue
}

func (*workerEventFetcher) Repository(context.Context, string, string, string) (githubsync.Repository, error) {
	return githubsync.Repository{
		NodeID: "R_exampleNode", ID: 12345, FullName: "example-owner/example-repo",
	}, nil
}

func (f *workerEventFetcher) Issues(context.Context, githubsync.Binding, *time.Time) ([]githubsync.Issue, error) {
	return append([]githubsync.Issue(nil), f.issues...), nil
}

func (*workerEventFetcher) Comments(context.Context, githubsync.Binding, int) ([]githubsync.Comment, error) {
	return nil, nil
}

func (*workerEventFetcher) ParentData(context.Context, githubsync.Binding) (githubsync.ParentData, error) {
	return githubsync.ParentData{Scan: githubsync.ParentScanUnsupported}, nil
}

// TestServiceRunExpiresTimedAssignments pins the embedded-service worker
// list: the assignment sweeper must run under Service.Run exactly as it does
// in the standalone daemon, or an already-expired timed assignment survives
// forever in a mounted service.
func TestServiceRunExpiresTimedAssignments(t *testing.T) {
	ctx := context.Background()
	service, err := newService(ctx, Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: AuthConfig{TrustCallerAuthentication: true},
	}, serviceDeps{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	project, err := service.store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	issue, _, err := service.store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "expired assignment",
		Author:    "operator",
	})
	require.NoError(t, err)
	seeded, err := service.store.ClaimOwner(ctx, db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "spoke-agent", TTL: time.Minute,
		Now: time.Now().UTC().Add(-2 * time.Minute),
	})
	require.NoError(t, err)
	require.True(t, seeded.Changed)

	sub := service.broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()

	runCtx, cancelRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()

	var expired *db.Event
	deadline := time.After(5 * time.Second)
	for expired == nil {
		select {
		case msg := <-sub.Ch:
			if msg.Kind == daemon.StreamKindEvent && msg.Event != nil &&
				msg.Event.Type == "issue.assignment_expired" {
				expired = msg.Event
			}
		case <-deadline:
			require.FailNow(t, "assignment expiry event was never broadcast")
		}
	}
	require.NotNil(t, expired.IssueID)
	assert.Equal(t, issue.ID, *expired.IssueID)

	stored, err := service.store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.Owner)
	assert.Nil(t, stored.AssignmentExpiresOn)

	events, err := service.store.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: project.ID, Types: []string{"issue.assignment_expired"}, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, expired.ID, events[0].ID)

	cancelRun()
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Run did not stop after cancellation")
	}
}

// TestServiceWorkerEventsReachBroadcasterAndHooks pins the invariant the
// mounted service used to break: an event a background worker produces must
// reach the SSE broadcaster *and* the hook sink. The federation pull callback
// broadcast only, so hook consumers never saw federated events.
func TestServiceWorkerEventsReachBroadcasterAndHooks(t *testing.T) {
	ctx := context.Background()
	updated := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	fetcher := &workerEventFetcher{issues: []githubsync.Issue{{
		ID: 101, NodeID: "I_example", Number: 1,
		HTMLURL:   "https://github.example/example-owner/example-repo/issues/1",
		Title:     "imported by the worker",
		Body:      "body",
		State:     "open",
		User:      &githubsync.User{Login: "author"},
		CreatedAt: &updated, UpdatedAt: &updated,
	}}}
	service, err := newService(ctx, Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: AuthConfig{TrustCallerAuthentication: true},
	}, serviceDeps{gitHubSyncFetcher: fetcher})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	sink := &recordingWorkerSink{}
	service.publish = daemon.NewEventPublisher(service.broadcaster, sink)
	sub := service.broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()

	project, err := service.store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	titlePrefix := true
	bindingConfig, err := githubsync.EncodeConfig(githubsync.Config{
		Host: "github.com", Owner: "example-owner", Repo: "example-repo",
		RepoID: 12345, TitlePrefix: &titlePrefix,
	})
	require.NoError(t, err)
	_, err = service.store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
		ProjectID: project.ID, Provider: "github", SourceKey: "github:R_exampleNode",
		RemoteID: "R_exampleNode", DisplayName: "example-owner/example-repo",
		Config: bindingConfig, IntervalSeconds: 300,
	})
	require.NoError(t, err)

	runCtx, cancelRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()

	var broadcastID int64
	select {
	case msg := <-sub.Ch:
		require.Equal(t, daemon.StreamKindEvent, msg.Kind)
		require.NotNil(t, msg.Event)
		broadcastID = msg.Event.ID
		assert.Equal(t, project.ID, msg.ProjectID)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "worker import event was never broadcast")
	}

	require.Eventually(t, func() bool {
		return len(sink.ids()) > 0
	}, 5*time.Second, 10*time.Millisecond, "worker import event never reached the hook sink")
	assert.Contains(t, sink.ids(), broadcastID,
		"the broadcast event id must also appear on the hook sink")

	cancelRun()
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Run did not stop after cancellation")
	}
}
