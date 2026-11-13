package federation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// repeatedAdoptionCopy is another Kata home that holds its own copy of an
// issue the hub project may already hold, and adopts its project into the hub.
type repeatedAdoptionCopy struct {
	env       *testenv.Env
	projectID int64
	creds     config.FederationCredential
}

func newRepeatedAdoptionCopy(t *testing.T, hub *testenv.Env, hubProject db.Project, issueUID, title, comment string, updatedAt time.Time) repeatedAdoptionCopy {
	t.Helper()
	ctx := t.Context()
	env := testenv.New(t)
	local, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: local.ID, UID: issueUID, Title: title, Author: "worker"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: comment})
	require.NoError(t, err)
	// The copy's updated_at is what the hub compares, so pin it.
	_, err = env.DB.ExecContext(ctx, `UPDATE issues SET updated_at=? WHERE id=?`, updatedAt.UTC().Format(db.EventTimestampFormat), issue.ID)
	require.NoError(t, err)

	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: env.DB.InstanceUID(), ProjectID: &hubProject.ID,
		Capabilities: "pull,push", Actor: "worker", AllowAdoptionSnapshotAuthors: true,
	})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)
	var replica api.CreateFederationReplicaResponseBody
	postJSON(t, env.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url": hub.URL, "hub_project_id": hubProject.ID, "hub_project_uid": hubProject.UID,
		"project_name": local.Name, "replay_horizon_event_id": hubBinding.ReplayHorizonEventID,
		"token": grant.Token, "capabilities": "pull,push", "actor": "worker",
		"push_enabled": true, "adopt_existing": true,
	}, &replica)
	require.True(t, replica.Adopted)
	return repeatedAdoptionCopy{
		env: env, projectID: replica.Project.ID,
		creds: config.FederationCredential{HubURL: hub.URL, HubProjectID: hubProject.ID, Token: grant.Token, Capabilities: "pull,push"},
	}
}

// ownSnapshots lists the adoption snapshots of the issue that this home still
// holds in its own log.
func (c repeatedAdoptionCopy) ownSnapshots(t *testing.T, issueUID string) []db.Event {
	t.Helper()
	events, err := c.env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: c.projectID, IssueUID: issueUID, Types: []string{"issue.snapshot"}, Limit: 10})
	require.NoError(t, err)
	var own []db.Event
	for _, event := range events {
		if event.OriginInstanceUID == c.env.DB.InstanceUID() {
			own = append(own, event)
		}
	}
	return own
}

func (c repeatedAdoptionCopy) sync(t *testing.T) {
	t.Helper()
	// Two cycles: the first pushes the adoption, the second pulls what the hub
	// wrote in answer to it.
	for range 2 {
		binding, err := c.env.DB.FederationBindingByProject(t.Context(), c.projectID)
		require.NoError(t, err)
		require.NoError(t, SyncFederationOnce(t.Context(), c.env.DB, binding, c.creds))
	}
}

func repeatedAdoptionState(t *testing.T, store db.Storage, issueUID string) (string, []string) {
	t.Helper()
	issue, err := store.IssueByUID(t.Context(), issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := store.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	bodies := make([]string, 0, len(comments))
	for _, comment := range comments {
		bodies = append(bodies, comment.Body)
	}
	return issue.Title, bodies
}

// A hub project already holds an issue. Other homes adopt their own copy of
// it into the same project. A newer copy updates the hub, an older or
// identical copy leaves no trace on it, and every home ends up with the hub's
// state.
func TestSyncFederationRepeatedAdoptionConvergesOnTheNewestCopy(t *testing.T) {
	ctx := t.Context()
	hub := testenv.New(t)
	hubProject := createFederatedHubForPush(t, hub)

	seed, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hubProject.ID, Title: "hub copy", Author: "worker"})
	require.NoError(t, err)
	_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: seed.ID, Author: "worker", Body: "hub comment"})
	require.NoError(t, err)
	seed, err = hub.DB.IssueByUID(ctx, seed.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	// The hub's own history gives the issue its time, so the copies are placed
	// around it.
	winning := seed.UpdatedAt.UTC().Truncate(time.Millisecond).Add(time.Hour)

	newer := newRepeatedAdoptionCopy(t, hub, hubProject, seed.UID, "newer copy", "newer comment", winning)
	newer.sync(t)
	title, comments := repeatedAdoptionState(t, hub.DB, seed.UID)
	require.Equal(t, "newer copy", title, "a newer copy updates the hub")
	require.ElementsMatch(t, []string{"hub comment", "newer comment"}, comments, "comments merge by UID")
	updated, err := hub.DB.IssueByUID(ctx, seed.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.True(t, winning.Equal(updated.UpdatedAt), "the hub issue takes the winning copy's time, got %s", updated.UpdatedAt)
	title, comments = repeatedAdoptionState(t, newer.env.DB, seed.UID)
	require.Equal(t, "newer copy", title)
	require.ElementsMatch(t, []string{"hub comment", "newer comment"}, comments, "the pushing home receives the hub's comments")
	require.Empty(t, newer.ownSnapshots(t, seed.UID), "the pushing home drops the snapshot the hub did not store")
	snapshots, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: hubProject.ID, IssueUID: seed.UID, Types: []string{"issue.snapshot"}, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, snapshots, "the hub stores no snapshot for an issue it already holds")

	lastEvent, err := hub.DB.MaxEventID(ctx)
	require.NoError(t, err)
	for name, updatedAt := range map[string]time.Time{"identical": winning, "older": winning.Add(-2 * time.Hour)} {
		stale := newRepeatedAdoptionCopy(t, hub, hubProject, seed.UID, name+" copy", name+" comment", updatedAt)
		stale.sync(t)
		after, err := hub.DB.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, lastEvent, after, "an %s copy writes nothing on the hub", name)
		title, comments = repeatedAdoptionState(t, hub.DB, seed.UID)
		require.Equal(t, "newer copy", title)
		require.ElementsMatch(t, []string{"hub comment", "newer comment"}, comments)
		title, comments = repeatedAdoptionState(t, stale.env.DB, seed.UID)
		require.Equal(t, "newer copy", title, "the home that pushed an %s copy takes the hub's state", name)
		require.ElementsMatch(t, []string{"hub comment", "newer comment"}, comments)
		require.Empty(t, stale.ownSnapshots(t, seed.UID))
	}
}

// A home adopts a newer copy and then edits the issue before its first sync.
// The edit is later than the copy, so it must survive on the hub and on the
// home, whatever order the hub writes its events in.
func TestSyncFederationRepeatedAdoptionKeepsAnEditMadeAfterTheSnapshot(t *testing.T) {
	ctx := t.Context()
	hub := testenv.New(t)
	hubProject := createFederatedHubForPush(t, hub)
	seed, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hubProject.ID, Title: "hub copy", Author: "worker"})
	require.NoError(t, err)

	// The copy is newer than the hub issue and older than the edit below.
	copyTime := seed.UpdatedAt.UTC().Truncate(time.Millisecond).Add(time.Millisecond)
	home := newRepeatedAdoptionCopy(t, hub, hubProject, seed.UID, "imported", "home comment", copyTime)
	local, err := home.env.DB.IssueByUID(ctx, seed.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	edited := "edited afterward"
	_, _, _, err = home.env.DB.EditIssue(ctx, db.EditIssueParams{IssueID: local.ID, Title: &edited, Actor: "worker"})
	require.NoError(t, err)

	home.sync(t)
	// The edit names only the title. Everything else the copy brought stays,
	// on the hub and on the home after it drops its own snapshot.
	title, comments := repeatedAdoptionState(t, hub.DB, seed.UID)
	require.Equal(t, "edited afterward", title)
	require.Equal(t, []string{"home comment"}, comments)
	title, comments = repeatedAdoptionState(t, home.env.DB, seed.UID)
	require.Equal(t, "edited afterward", title)
	require.Equal(t, []string{"home comment"}, comments)
	require.Empty(t, home.ownSnapshots(t, seed.UID))
}

// The hub names the snapshots it did not store, and the pushing home deletes
// them. Until the hub's history for the issue has arrived, the home's own
// snapshot is the only record it has of the issue, so it must keep it. Once
// the history is there, the home drops the snapshot and shows the hub's state
// without waiting for another event.
func TestSyncFederationRepeatedAdoptionKeepsItsSnapshotUntilTheHubHistoryArrives(t *testing.T) {
	ctx := t.Context()
	hub := testenv.New(t)
	hubProject := createFederatedHubForPush(t, hub)
	seed, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hubProject.ID, Title: "hub copy", Author: "worker"})
	require.NoError(t, err)
	older := seed.UpdatedAt.UTC().Truncate(time.Millisecond).Add(-time.Hour)
	home := newRepeatedAdoptionCopy(t, hub, hubProject, seed.UID, "older copy", "home comment", older)

	// Stop the first cycle after its push, before any pull.
	binding, err := home.env.DB.FederationBindingByProject(ctx, home.projectID)
	require.NoError(t, err)
	client, err := NewClient(ctx, home.creds.HubURL, home.creds.Token, clientpkg.Opts{})
	require.NoError(t, err)
	require.ErrorIs(t, PushDirectSpoke(ctx, home.env.DB, binding, hubProject.ID, client), db.ErrFederationPushAwaitsPull)
	require.Len(t, home.ownSnapshots(t, seed.UID), 1, "the home keeps its snapshot while it has no other history of the issue")
	title, _ := repeatedAdoptionState(t, home.env.DB, seed.UID)
	require.Equal(t, "older copy", title)
	waiting, err := home.env.DB.FederationBindingByProject(ctx, home.projectID)
	require.NoError(t, err)
	require.Equal(t, binding.PushCursorEventID, waiting.PushCursorEventID, "the push is not acknowledged yet")
	status, err := home.env.DB.FederationSyncStatusByProject(ctx, home.projectID)
	require.NoError(t, err)
	require.NotNil(t, status.LastError, "federation status shows why the push has not finished")
	require.Contains(t, *status.LastError, "waits for the hub's history")

	// The rebuild changes rows without a pulled event, so open clients learn
	// of it from a reset the sync publishes.
	var published []db.Event
	for range 2 {
		binding, err := home.env.DB.FederationBindingByProject(ctx, home.projectID)
		require.NoError(t, err)
		require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, home.env.DB, binding, home.creds, clientOptsWithDefault(clientpkg.Opts{}), func(_ int64, events []db.Event) {
			published = append(published, events...)
		}))
	}
	require.Empty(t, home.ownSnapshots(t, seed.UID))
	cursor, err := home.env.DB.MaxEventID(ctx)
	require.NoError(t, err)
	require.Contains(t, published, db.Event{ID: cursor, ProjectID: home.projectID, Type: db.ProjectScopeResetEventType},
		"the sync publishes the reset that follows the rebuild")
	title, comments := repeatedAdoptionState(t, home.env.DB, seed.UID)
	require.Equal(t, "hub copy", title, "the home shows the hub's state as soon as it drops its snapshot")
	require.Empty(t, comments)
	status, err = home.env.DB.FederationSyncStatusByProject(ctx, home.projectID)
	require.NoError(t, err)
	require.NotNil(t, status.LastPushSuccessAt)
	require.NotNil(t, status.LastErrorAt)
	require.False(t, status.LastPushSuccessAt.Before(*status.LastErrorAt), "the finished push is recorded after the wait")
}
