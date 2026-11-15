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

func TestSyncFederationFormerTransitIdentitySurvivesSourceReplay(t *testing.T) {
	for _, operation := range []string{"purge", "merge", "current-destination-purge"} {
		t.Run(operation, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "transit-source", "transit-middle", "transit-target")
			receiver := newMoveDeliverySpoke(t, hub, projects)
			issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "source replay through transit", Author: "worker"})
			require.NoError(t, err)
			for i := range projects {
				require.NoError(t, receiver.sync(t, i))
			}
			first, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			destination := 2
			if operation == "current-destination-purge" {
				destination = 1
			} else {
				_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[1].ID, ToProjectID: projects[2].ID, IfMatchRev: first.Issue.Revision, Actor: "worker"})
				require.NoError(t, err)
			}
			require.NoError(t, receiver.sync(t, destination), "the destination syncs pending moves while source stays behind")
			for index := 1; index <= destination; index++ {
				_, err = receiver.env.DB.LeaveFederationReplica(ctx, receiver.projects[index].ID)
				require.NoError(t, err)
			}
			standalone, err := receiver.env.DB.CreateProject(ctx, "transit-cleanup-standalone")
			require.NoError(t, err)
			path := "/api/v1/projects/" + strconv.FormatInt(standalone.ID, 10) + "/merge"
			payload := `{"actor":"worker","source_project_id":` + strconv.FormatInt(receiver.projects[1].ID, 10) + `}`
			if operation != "merge" {
				_, _, err = receiver.env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: receiver.projects[1].ID, Actor: "worker", Force: true})
				require.NoError(t, err)
				path = "/api/v1/projects/" + strconv.FormatInt(receiver.projects[1].ID, 10) + "/actions/purge"
				payload = `{"actor":"worker"}`
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, receiver.env.URL+path, strings.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Kata-Confirm", "PURGE "+receiver.projects[1].Name)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusConflict, resp.StatusCode, "%s", body)
			require.Contains(t, string(body), "identity is required by federation history")
			require.NoError(t, receiver.sync(t, 0), "the still-joined source must resolve its delayed A-to-B move envelope")
			current, err := receiver.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, receiver.projects[destination].ID, current.ProjectID, "source replay preserves detached destination state")
			_, err = receiver.env.DB.LeaveFederationReplica(ctx, receiver.projects[0].ID)
			require.NoError(t, err)
			if operation != "merge" {
				_, err = receiver.env.DB.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: receiver.projects[1].ID, Actor: "worker"})
			} else {
				_, err = receiver.env.DB.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: receiver.projects[1].ID, TargetProjectID: standalone.ID, Actor: "worker"})
			}
			require.NoError(t, err, "cleanup is allowed after all relevant streams detach")
		})
	}
}
