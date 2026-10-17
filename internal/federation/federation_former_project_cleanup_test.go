package federation

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSyncFederationFormerProjectCleanupRetainsDelayedEnvelopeIdentity(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "cleanup-source", "cleanup-target")
	receiver, sender := newMoveDeliverySpoke(t, hub, projects), newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "delayed cleanup history", Author: "worker"})
	require.NoError(t, err)
	for _, spoke := range []moveDeliverySpoke{receiver, sender} {
		for i := range projects {
			require.NoError(t, spoke.sync(t, i))
		}
	}
	stale, err := sender.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, receiver.sync(t, i))
	}
	_, err = receiver.env.DB.LeaveFederationReplica(ctx, receiver.projects[0].ID)
	require.NoError(t, err)
	_, _, err = receiver.env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: receiver.projects[0].ID, Actor: "worker"})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, receiver.env.URL+"/api/v1/projects/"+strconv.FormatInt(receiver.projects[0].ID, 10)+"/actions/purge", strings.NewReader(`{"actor":"worker"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kata-Confirm", "PURGE "+receiver.projects[0].Name)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusConflict, resp.StatusCode, "%s", body)
	require.Contains(t, string(body), "identity is required by federation history")
	_, commentEvent, err := sender.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: stale.ID, Author: "worker", Body: "delayed source comment"})
	require.NoError(t, err)
	require.NotEmpty(t, commentEvent.UID)
	require.NoError(t, sender.sync(t, 0))
	require.NoError(t, receiver.sync(t, 1), "the surviving destination must consume delayed source envelopes")
	current, err := receiver.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := receiver.env.DB.CommentsByIssue(ctx, current.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "delayed source comment", comments[0].Body)
	events, err := receiver.env.DB.EventsByUIDs(ctx, receiver.projects[0].ID, []string{commentEvent.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, projects[0].UID, events[0].ProjectUID)
	require.Equal(t, commentEvent.ContentHash, events[0].ContentHash)
	_, err = receiver.env.DB.LeaveFederationReplica(ctx, receiver.projects[1].ID)
	require.NoError(t, err)
	_, err = receiver.env.DB.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: receiver.projects[0].ID, Actor: "worker"})
	require.NoError(t, err, "after destination teardown cleanup ends historical compatibility")
}
