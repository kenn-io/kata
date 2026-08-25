package daemon

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type expiryBoundaryStore struct {
	db.Storage
	onExpire func()
}

func (s *expiryBoundaryStore) ExpireTimedClaimsForProject(
	ctx context.Context,
	projectID int64,
	now time.Time,
	limit int,
) ([]db.Event, error) {
	s.onExpire()
	return s.Storage.ExpireTimedClaimsForProject(ctx, projectID, now, limit)
}

type forkAwareExpirySink struct {
	ordinary []db.Event
	from     []db.Event
	admitted []bool
}

func (s *forkAwareExpirySink) Enqueue(evt db.Event) {
	s.ordinary = append(s.ordinary, evt)
}

func (s *forkAwareExpirySink) EnqueueFrom(evt db.Event, acquire activity.Admission) {
	lease, admitted := acquire()
	s.from = append(s.from, evt)
	s.admitted = append(s.admitted, admitted)
	if lease != nil {
		lease.Release()
	}
}

func TestTimedClaimSweeperSkipsPassWhenDrainAdmissionIsClosed(t *testing.T) {
	controller := NewIdleController(time.Minute, nil)
	controller.Start()
	controller.Stop()
	sweeper := NewTimedClaimSweeper(nil, NewEventPublisher(nil, nil))
	sweeper.IdleAdmission = controller.WaitableDrainAdmission()

	require.NoError(t, sweeper.RunOnce(context.Background(), time.Now()))
}

func TestTimedClaimSweeperRetriesImmediatelyWhenDrainAdmissionReopens(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	reopened := make(chan struct{})
	completed := make(chan struct{})
	var attempts atomic.Int32
	sweeper := NewTimedClaimSweeper(store, NewEventPublisher(nil, nil))
	sweeper.Interval = time.Hour
	sweeper.IdleAdmission = func() (*activity.Lease, bool, <-chan struct{}) {
		if attempts.Add(1) == 1 {
			return nil, false, reopened
		}
		return activity.NewLease(func() { close(completed) }, nil), true, nil
	}

	done := make(chan error, 1)
	go func() { done <- sweeper.Run(ctx) }()
	require.Eventually(t, func() bool { return attempts.Load() == 1 }, time.Second, time.Millisecond)

	close(reopened)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("timed-claim sweep did not complete after drain admission reopened")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestTimedClaimSweeperWaitsForCancellationAfterTerminalDrainDenial(t *testing.T) {
	var attempts atomic.Int32
	sweeper := NewTimedClaimSweeper(nil, NewEventPublisher(nil, nil))
	sweeper.Interval = 5 * time.Millisecond
	sweeper.IdleAdmission = func() (*activity.Lease, bool, <-chan struct{}) {
		attempts.Add(1)
		return nil, false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sweeper.Run(ctx) }()
	require.Eventually(t, func() bool { return attempts.Load() >= 1 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(1), attempts.Load(), "terminal denial must not be polled")

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestTimedClaimSweeperForksBlockedIdleLeaseForCausedHook(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	project, err := store.CreateProject(ctx, "claim-sweeper-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "expired timed claim",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(),
			Holder:            "claim-holder",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now.Add(-2 * time.Minute),
	})
	require.NoError(t, err)

	clock := newIdleTestClock(now)
	idle := make(chan struct{}, 1)
	controller := newIdleControllerWithClock(time.Minute, func() { idle <- struct{}{} }, clock)
	controller.Start()
	boundaryStore := &expiryBoundaryStore{Storage: store, onExpire: func() {
		clock.Advance(time.Minute)
		require.Equal(t, IdleStateBlocked, controller.Snapshot().State)
	}}
	broadcaster := NewEventBroadcaster()
	sub := broadcaster.Subscribe(SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	sink := &forkAwareExpirySink{}
	sweeper := NewTimedClaimSweeper(boundaryStore, NewEventPublisher(broadcaster, sink))
	sweeper.IdleAdmission = controller.WaitableDrainAdmission()

	require.NoError(t, sweeper.RunOnce(ctx, now))

	select {
	case msg := <-sub.Ch:
		require.NotNil(t, msg.Event)
		require.Equal(t, "claim.expired", msg.Event.Type)
		require.Equal(t, project.ID, msg.ProjectID)
	case <-time.After(time.Second):
		t.Fatal("timed-claim expiry was not broadcast")
	}
	require.Empty(t, sink.ordinary)
	require.Len(t, sink.from, 1)
	require.Equal(t, "claim.expired", sink.from[0].Type)
	require.Equal(t, project.ID, sink.from[0].ProjectID)
	require.Equal(t, []bool{true}, sink.admitted)
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("idle callback did not run after the parent sweep and caused hook released")
	}
}
