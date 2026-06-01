package daemon_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

func TestTimedClaimSweeperExpiresOnlyHubClaimsAndFansOutEvents(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	now := time.Date(2026, 5, 23, 18, 0, 0, 0, time.UTC)
	hubProject, hubIssue := createClaimHubIssue(t, env)
	spokeProject, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	spokeIssue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spokeProject.ID,
		Title:     "spoke timed",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:1",
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: hubProject.ID,
		IssueRef:  hubIssue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "hub-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now.Add(-2 * time.Minute),
	})
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: spokeProject.ID,
		IssueRef:  spokeIssue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "spoke-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now.Add(-2 * time.Minute),
	})
	require.NoError(t, err)

	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()
	hookSink := &captureHookSink{}
	sweeper := daemon.NewTimedClaimSweeper(env.DB, broadcaster, hookSink)

	require.NoError(t, sweeper.RunOnce(ctx, now))

	msg := receiveMsg(t, sub.Ch, time.Second, "claim expired broadcast")
	require.NotNil(t, msg.Event)
	assert.Equal(t, "claim.expired", msg.Event.Type)
	assert.Equal(t, hubProject.ID, msg.ProjectID)
	require.Len(t, hookSink.events, 1)
	assert.Equal(t, "claim.expired", hookSink.events[0].Type)

	var hubReleased, spokeReleased int
	require.NoError(t, env.DB.QueryRowContext(ctx,
		`SELECT released_at IS NOT NULL FROM issue_claims WHERE issue_uid = ?`, hubIssue.UID).Scan(&hubReleased))
	require.NoError(t, env.DB.QueryRowContext(ctx,
		`SELECT released_at IS NOT NULL FROM issue_claims WHERE issue_uid = ?`, spokeIssue.UID).Scan(&spokeReleased))
	assert.Equal(t, 1, hubReleased)
	assert.Equal(t, 0, spokeReleased, "sweeper must not expire spoke cache claims")
}

func TestTimedClaimSweeperSkipsArchivedHubBindings(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	now := time.Date(2026, 5, 23, 18, 0, 0, 0, time.UTC)
	project, issue := createClaimHubIssue(t, env)
	_, err := env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "hub-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now.Add(-2 * time.Minute),
	})
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID,
		Actor:     "tester",
		Force:     true,
	})
	require.NoError(t, err)

	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	hookSink := &captureHookSink{}
	sweeper := daemon.NewTimedClaimSweeper(env.DB, broadcaster, hookSink)

	require.NoError(t, sweeper.RunOnce(ctx, now))
	assertNoReceive(t, sub.Ch, 100*time.Millisecond, "archived project should not broadcast claim expiry")
	require.Empty(t, hookSink.events)

	var released int
	require.NoError(t, env.DB.QueryRowContext(ctx,
		`SELECT released_at IS NOT NULL FROM issue_claims WHERE issue_uid = ?`, issue.UID).Scan(&released))
	assert.Equal(t, 0, released, "archived project claim should remain untouched")
}

func TestTimedClaimSweeperRunReportsPassErrorsToOnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	require.NoError(t, store.Close())
	errCh := make(chan error, 1)
	observed := make(chan error, 1)
	sweeper := daemon.NewTimedClaimSweeper(store, nil, nil)
	sweeper.Interval = time.Hour
	sweeper.OnError = func(err error) {
		observed <- err
		cancel()
	}

	go func() {
		errCh <- sweeper.Run(ctx)
	}()

	select {
	case err := <-observed:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("timed claim sweeper did not report RunOnce error")
	}
	require.ErrorIs(t, <-errCh, context.Canceled)
}

type captureHookSink struct {
	events []db.Event
}

func (s *captureHookSink) Enqueue(evt db.Event) {
	s.events = append(s.events, evt)
}
