package federation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
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
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Millisecond)

	seed, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hubProject.ID, Title: "hub copy", Author: "worker"})
	require.NoError(t, err)
	_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: seed.ID, Author: "worker", Body: "hub comment"})
	require.NoError(t, err)
	_, err = hub.DB.ExecContext(ctx, `UPDATE issues SET updated_at=? WHERE id=?`, base.Format(db.EventTimestampFormat), seed.ID)
	require.NoError(t, err)

	newer := newRepeatedAdoptionCopy(t, hub, hubProject, seed.UID, "newer copy", "newer comment", base.Add(time.Hour))
	newer.sync(t)
	title, comments := repeatedAdoptionState(t, hub.DB, seed.UID)
	require.Equal(t, "newer copy", title, "a newer copy updates the hub")
	require.ElementsMatch(t, []string{"hub comment", "newer comment"}, comments, "comments merge by UID")
	title, comments = repeatedAdoptionState(t, newer.env.DB, seed.UID)
	require.Equal(t, "newer copy", title)
	require.ElementsMatch(t, []string{"hub comment", "newer comment"}, comments, "the pushing home receives the hub's comments")
	snapshots, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: hubProject.ID, IssueUID: seed.UID, Types: []string{"issue.snapshot"}, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, snapshots, "the hub stores no snapshot for an issue it already holds")

	lastEvent, err := hub.DB.MaxEventID(ctx)
	require.NoError(t, err)
	for name, updatedAt := range map[string]time.Time{"identical": base.Add(time.Hour), "older": base.Add(-time.Hour)} {
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
	}
}
