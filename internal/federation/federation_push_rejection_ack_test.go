package federation

import (
	"encoding/json/v2"
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

func TestFederationPushRetainsUnrecognizedRejection(t *testing.T) {
	ctx := t.Context()
	spoke := testenv.New(t)
	project, err := spoke.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, event, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Pending", Author: "worker"})
	require.NoError(t, err)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.MarshalWrite(w, api.FederationIngestEventsBody{
			PushCursorEventID: event.ID, RejectedEvents: []db.FederationEventRejection{{EventUID: "unknown", SourceEventID: event.ID}},
		}))
	}))
	t.Cleanup(hub.Close)
	binding, err := spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 1, HubProjectUID: project.UID, PushEnabled: true, Enabled: true, Actor: "worker"})
	require.NoError(t, err)
	client, err := NewClient(ctx, hub.URL, "token", clientpkg.Opts{})
	require.NoError(t, err)
	require.Error(t, pushDirectSpoke(ctx, spoke.DB, &binding, 1, client, nil, newMoveCredentialStore(), clientpkg.Opts{}))
	retained, err := spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Less(t, retained.PushCursorEventID, event.ID)
	pending, err := spoke.DB.PendingFederationPushEvents(ctx, project.ID, spoke.DB.InstanceUID(), retained.PushCursorEventID, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, event.UID, pending[0].UID)
}

func TestFederationPushRejectsInvalidAcknowledgments(t *testing.T) {
	batch := []db.Event{{ID: 1, UID: "one"}, {ID: 2, UID: "two"}}
	for name, ack := range map[string]api.FederationIngestEventsBody{
		"unknown event":           {Accepted: 1, PushCursorEventID: 2, RejectedEvents: []db.FederationEventRejection{{EventUID: "other", SourceEventID: 1}}},
		"wrong source id":         {Accepted: 1, PushCursorEventID: 2, RejectedEvents: []db.FederationEventRejection{{EventUID: "one", SourceEventID: 2}}},
		"duplicate rejection":     {PushCursorEventID: 2, RejectedEvents: []db.FederationEventRejection{{EventUID: "one", SourceEventID: 1}, {EventUID: "one", SourceEventID: 1}}},
		"missing accounting":      {Accepted: 1, PushCursorEventID: 2},
		"negative count":          {Accepted: -1, Duplicates: 3, PushCursorEventID: 2},
		"rejection beyond cursor": {Accepted: 1, PushCursorEventID: 1, RejectedEvents: []db.FederationEventRejection{{EventUID: "two", SourceEventID: 2}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validateFederationPushAcknowledgment(batch, ack)
			require.Error(t, err)
		})
	}
}

func TestFederationPushReportsUnknownRejectionReason(t *testing.T) {
	message := federationEventRejectionMessage([]db.FederationEventRejection{{Code: "future_reason", Message: "future detail"}}, 1, 0)
	require.Contains(t, message, "future_reason")
	require.NotContains(t, message, "need project access")
}
