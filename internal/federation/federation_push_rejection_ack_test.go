package federation

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

func TestFederationEventRejectionOptInToOldHub(t *testing.T) {
	oldHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "1", r.Header.Get(db.FederationEventRejectionsHeader))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":403,"error":{"code":"auth_invalid","message":"federation credential is not currently authorized"}}`))
	}))
	t.Cleanup(oldHub.Close)
	client, err := NewClient(t.Context(), oldHub.URL, "example-token", clientpkg.Opts{})
	require.NoError(t, err)
	ack, err := client.IngestProjectEventsWithOptions(t.Context(), 1, nil, IngestProjectEventsOptions{AllowEventRejections: true})
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusForbidden, status.StatusCode)
	require.Zero(t, ack.PushCursorEventID, "an older hub's atomic failure acknowledges no prefix")
	require.Empty(t, ack.RejectedEvents)
}

func TestFederationPushRejectsUnsubmittedRejectionAcknowledgments(t *testing.T) {
	batch := []db.Event{{ID: 1, UID: "submitted-event"}, {ID: 2, UID: "later-event"}}
	rejection := db.FederationEventRejection{SourceEventID: 1, EventUID: "submitted-event", IssueUID: "issue-uid", CurrentProjectUID: "project-uid", CurrentProjectName: "target-project", Code: db.FederationProjectNotEnrolledCode}
	for _, kind := range []string{"wrong-uid", "beyond-prefix"} {
		t.Run(kind, func(t *testing.T) {
			ack := api.FederationIngestEventsBody{PushCursorEventID: 1, RejectedEvents: []db.FederationEventRejection{rejection}}
			switch kind {
			case "wrong-uid":
				ack.RejectedEvents[0].EventUID = "unsubmitted-event"
			case "beyond-prefix":
				ack.RejectedEvents[0].EventUID = "later-event"
				ack.RejectedEvents[0].SourceEventID = 2
			}
			_, err := validateFederationPushAcknowledgment(batch, ack)
			require.Error(t, err)
		})
	}
}

func TestFederationPushRejectionPersistenceFailureRetainsEvents(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "retained-source", "retained-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved work", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, stale, err := spoke.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Retain across recording failure"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	good, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Accepted despite lost response", Author: "worker"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
	require.NoError(t, err)
	before := binding.PushCursorEventID
	client, err := NewClient(t.Context(), hub.URL, spoke.credentials[projects[0].UID].Token, clientpkg.Opts{})
	require.NoError(t, err)
	store := spoke.env.DB
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "before_spoke_push_cursor_advance=error")
	require.Error(t, pushDirectSpoke(t.Context(), store, &binding, projects[0].ID, client, nil, spoke.credentials, clientpkg.Opts{}))
	require.Equal(t, before, binding.PushCursorEventID)
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	require.NoError(t, pushDirectSpoke(t.Context(), store, &binding, projects[0].ID, client, nil, spoke.credentials, clientpkg.Opts{}))
	require.Less(t, binding.PushCursorEventID, stale.ID)
	_, err = hub.DB.IssueByUID(t.Context(), good.UID, db.IncludeDeletedNo)
	require.NoError(t, err, "response-lost replay keeps accepted independent work")
	retained, err := store.EventsByUIDs(t.Context(), binding.ProjectID, []string{stale.UID})
	require.NoError(t, err)
	require.Equal(t, stale.ContentHash, retained[0].ContentHash)
}
