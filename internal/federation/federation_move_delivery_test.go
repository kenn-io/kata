package federation

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type moveDeliverySpoke struct {
	env         *testenv.Env
	projects    []db.Project
	credentials moveCredentialStore
}

func newMoveDeliveryHub(t *testing.T, names ...string) (*testenv.Env, []db.Project) {
	t.Helper()
	hub := testenv.New(t)
	projects := make([]db.Project, len(names))
	for i, name := range names {
		var err error
		projects[i], err = hub.DB.CreateProject(t.Context(), name)
		require.NoError(t, err)
		_, err = hub.DB.EnableProjectFederation(t.Context(), projects[i].ID, "worker")
		require.NoError(t, err)
	}
	return hub, projects
}

func newMoveDeliverySpoke(t *testing.T, hub *testenv.Env, projects []db.Project) moveDeliverySpoke {
	t.Helper()
	spoke := moveDeliverySpoke{env: testenv.New(t), projects: make([]db.Project, len(projects)), credentials: newMoveCredentialStore()}
	for i, project := range projects {
		var err error
		spoke.projects[i], err = spoke.env.DB.CreateProjectWithUID(t.Context(), project.Name, project.UID)
		require.NoError(t, err)
		_, err = spoke.env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: spoke.projects[i].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		grant, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker"})
		require.NoError(t, err)
		spoke.credentials[project.UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token, Actor: "worker"}
	}
	return spoke
}

func (s moveDeliverySpoke) sync(t *testing.T, index int) error {
	t.Helper()
	binding, err := s.env.DB.FederationBindingByProject(t.Context(), s.projects[index].ID)
	if err != nil {
		return err
	}
	return SyncFederationOnce(t.Context(), s.env.DB, binding, s.credentials[s.projects[index].UID], s.credentials)
}

func (s moveDeliverySpoke) move(t *testing.T, issue db.Issue, from, to int) db.Issue {
	t.Helper()
	moved, err := s.env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: s.projects[from].ID, ToProjectID: s.projects[to].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	return moved.Issue
}

func TestSyncFederationUnsyncedCreateRoundTrip(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "roundtrip-source", "roundtrip-target")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "unsynced round trip", Author: "worker"})
	require.NoError(t, err)
	issue = spoke.move(t, issue, 0, 1)
	spoke.move(t, issue, 1, 0)
	require.NoError(t, spoke.sync(t, 0), "an earlier creation in this batch satisfies the return move")
	require.NoError(t, spoke.sync(t, 1))
	require.NoError(t, spoke.sync(t, 0))
	got, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, projects[0].ID, got.ProjectID)
	require.Equal(t, "unsynced round trip", got.Title)
}

func TestSyncFederationRetriesReverseOrderOfflineMoves(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "chain-source", "chain-middle", "chain-target")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "offline chain", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, spoke.sync(t, i))
	}
	local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	local = spoke.move(t, local, 0, 1)
	spoke.move(t, local, 1, 2)
	err = spoke.sync(t, 2)
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	if status == nil {
		t.Fatal("missing hub status")
	}
	require.Equal(t, http.StatusNotFound, status.StatusCode, "an undelivered predecessor is retryable")
	_, err = spoke.env.DB.ActiveFederationQuarantine(t.Context(), spoke.projects[2].ID, db.FederationQuarantineDirectionPush)
	require.ErrorIs(t, err, db.ErrNotFound)
	binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[2].ID)
	require.NoError(t, err)
	require.True(t, binding.Enabled, "ordinary scheduling must retain the retrying destination")
	require.NoError(t, spoke.sync(t, 1))
	require.NoError(t, spoke.sync(t, 2), "ordinary retry should finish the chain")
	got, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, projects[2].ID, got.ProjectID)
}

func TestSyncFederationDelayedSourceCommentAfterLeave(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "delayed-source", "delayed-target")
	receiver := newMoveDeliverySpoke(t, hub, projects)
	sender := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "delayed source history", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, receiver.sync(t, i))
		require.NoError(t, sender.sync(t, i))
	}
	old, err := sender.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, _, err = sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: old.ID, Author: "worker", Body: "offline source comment"})
	require.NoError(t, err)
	local, err := receiver.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	receiver.move(t, local, 0, 1)
	require.NoError(t, receiver.sync(t, 1))
	_, err = receiver.env.DB.LeaveFederationReplica(t.Context(), receiver.projects[0].ID)
	require.NoError(t, err)
	require.NoError(t, receiver.credentials.DeleteFederationCredential(t.Context(), receiver.projects[0].UID))
	require.NoError(t, sender.sync(t, 0), "the delayed comment is accepted with its original project envelope")
	_, _, err = hub.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "destination follow-up"})
	require.NoError(t, err)
	require.NoError(t, receiver.sync(t, 1), "retained source identity must suffice after allowed source teardown")
	got, err := receiver.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, receiver.projects[1].ID, got.ProjectID)
	comments, err := receiver.env.DB.CommentsByIssue(t.Context(), got.ID)
	require.NoError(t, err)
	bodies := make([]string, 0, len(comments))
	for _, comment := range comments {
		bodies = append(bodies, comment.Body)
	}
	require.ElementsMatch(t, []string{"offline source comment", "destination follow-up"}, bodies)
}
