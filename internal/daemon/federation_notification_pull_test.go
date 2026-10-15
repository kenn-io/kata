package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// A federation pull replicates events verbatim: the spoke checks each payload
// against its content hash. Reader-specific notification projection must not
// rewrite them, even when the request's own access filter excludes the project.
func TestFederationPullKeepsNotificationPayloadsVerbatim(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "hub-project")
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, project.ID, "admin")
		require.NoError(t, err)
		team, _, err := store.CreateTeam(ctx, "project-team", "admin")
		require.NoError(t, err)
		_, err = store.SetTeamMembership(ctx, team.UID, "example-actor", true, "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{
			ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID},
		}, "admin")
		require.NoError(t, err)
		enrollment, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token: "project-events-enrollment", SpokeInstanceUID: federationTestSpokeUID,
			ProjectID: &project.ID, Capabilities: "pull", Actor: "example-actor",
		})
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Finding", Author: "example-actor"})
		require.NoError(t, err)
		comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "Finding"})
		require.NoError(t, err)
		notified, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "example-actor", Patch: map[string]jsontext.Value{
			"notify.d29ya2Vy": jsontext.Value(fmt.Sprintf(`{"from":"example-actor","message":"Inspect this finding","re":%q}`, comment.UID)),
		}})
		require.NoError(t, err)

		server := daemon.NewServer(daemon.ServerConfig{DB: store})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		request := httptest.NewRequest(http.MethodGet,
			projectPath(project.ID)+"/federation/events?after_id=0&limit=100", nil)
		request.Header.Set("Authorization", "Bearer "+enrollment.Token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())

		var polled api.PollEventsBody
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &polled))
		for _, event := range polled.Events {
			if event.EventUID == notified.Event.UID {
				require.JSONEq(t, notified.Event.Payload, string(event.Payload))
				return
			}
		}
		t.Fatalf("pull omitted the notification event: %s", response.Body.String())
	})
}
