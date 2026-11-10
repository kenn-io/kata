package federation

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/testenv"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

func TestFederationEventRejectionOptInToOldHub(t *testing.T) {
	oldHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "1", r.Header.Get(db.FederationEventRejectionsHeader))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":403,"error":{"code":"auth_invalid","message":"federation credential is not currently authorized"}}`))
	}))
	t.Cleanup(oldHub.Close)
	client, err := NewClient(t.Context(), oldHub.URL, "example-token", clientpkg.Opts{})
	require.NoError(t, err)
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(t.Context(), "old-hub-project")
	require.NoError(t, err)
	binding, err := spoke.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: oldHub.URL, HubProjectID: 1, HubProjectUID: project.UID, PushEnabled: true, Enabled: true, Actor: "worker"})
	require.NoError(t, err)
	_, _, err = spoke.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Pending work", Author: "worker"})
	require.NoError(t, err)
	err = pushDirectSpoke(t.Context(), spoke.DB, &binding, 1, client, nil, newMoveCredentialStore(), clientpkg.Opts{})
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusForbidden, status.StatusCode)
	retained, err := spoke.DB.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	require.Zero(t, retained.PushCursorEventID, "older-hub failure retains pending work")
	_, err = spoke.DB.ActiveFederationQuarantine(t.Context(), project.ID, db.FederationQuarantineDirectionPush)
	require.ErrorIs(t, err, db.ErrNotFound)
	syncStatus, err := spoke.DB.FederationSyncStatusByProject(t.Context(), project.ID)
	require.NoError(t, err)
	require.NotNil(t, syncStatus.LastError)
	require.Contains(t, *syncStatus.LastError, "credential is not currently authorized")
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
			require.EqualError(t, err, "hub returned invalid rejection details")
		})
	}
}
