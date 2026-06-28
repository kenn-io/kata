package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

func TestFederationClientPollProjectEvents(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		assert.Equal(t, "/api/v1/projects/42/federation/events", r.URL.Path)
		assert.Equal(t, "7", r.URL.Query().Get("after_id"))
		assert.Equal(t, "2", r.URL.Query().Get("limit"))
		require.NoError(t, json.NewEncoder(w).Encode(api.PollEventsBody{
			Events: []api.EventEnvelope{
				{EventID: 8, EventUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Type: "issue.snapshot"},
			},
			NextAfterID: 8,
		}))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(context.Background(), srv.URL, "hub-token", clientpkg.Opts{})
	require.NoError(t, err)
	body, err := client.PollProjectEvents(context.Background(), 42, 7, 2)
	require.NoError(t, err)

	assert.Equal(t, "Bearer hub-token", gotAuth)
	require.Len(t, body.Events, 1)
	assert.Equal(t, int64(8), body.Events[0].EventID)
	assert.Equal(t, int64(8), body.NextAfterID)
}

func TestFederationClientIngestProjectEvents(t *testing.T) {
	var gotAuth string
	var gotBody api.FederationIngestEventsRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/projects/42/federation/events:ingest", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		require.NoError(t, json.NewEncoder(w).Encode(api.FederationIngestEventsBody{
			Accepted:          1,
			Duplicates:        0,
			PushCursorEventID: 8,
		}))
	}))
	t.Cleanup(srv.Close)
	issueUID := "01HZNQ7VFPK1XGD8R5MABCD4EY"
	events := []api.FederationIngestEventEnvelope{{
		EventID:           8,
		EventUID:          "01HZNQ7VFPK1XGD8R5MABCD4EX",
		OriginInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		ProjectUID:        "01HZNQ7VFPK1XGD8R5MABCD4EP",
		ProjectName:       "kata",
		IssueUID:          &issueUID,
		Type:              "issue.created",
		Actor:             "spoke",
		HLCPhysicalMS:     1,
		HLCCounter:        0,
		ContentHash:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Payload:           json.RawMessage(`{"title":"pushed"}`),
	}}

	client, err := NewClient(context.Background(), srv.URL, "hub-token", clientpkg.Opts{})
	require.NoError(t, err)
	got, err := client.IngestProjectEventsWithOptions(context.Background(), 42, events, IngestProjectEventsOptions{
		AdoptionBaseline:           api.FederationAdoptionBaselineOpen,
		AdoptionBaselineEndEventID: 8,
	})
	require.NoError(t, err)

	assert.Equal(t, "Bearer hub-token", gotAuth)
	assert.Equal(t, db.CurrentSchemaVersion(), gotBody.SchemaVersion)
	assert.Equal(t, api.FederationAdoptionBaselineOpen, gotBody.AdoptionBaseline)
	assert.Equal(t, int64(8), gotBody.AdoptionBaselineEndEventID)
	require.Len(t, gotBody.Events, 1)
	assert.Equal(t, events[0].EventID, gotBody.Events[0].EventID)
	assert.Equal(t, events[0].EventUID, gotBody.Events[0].EventUID)
	assert.Equal(t, events[0].OriginInstanceUID, gotBody.Events[0].OriginInstanceUID)
	assert.Equal(t, events[0].ProjectUID, gotBody.Events[0].ProjectUID)
	assert.Equal(t, events[0].IssueUID, gotBody.Events[0].IssueUID)
	assert.Equal(t, events[0].Payload, gotBody.Events[0].Payload)
	assert.Equal(t, 1, got.Accepted)
	assert.Equal(t, 0, got.Duplicates)
	assert.Equal(t, int64(8), got.PushCursorEventID)
}

func TestFederationClientIngestProjectEventsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad batch", http.StatusConflict)
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(context.Background(), srv.URL, "hub-token", clientpkg.Opts{})
	require.NoError(t, err)
	_, err = client.IngestProjectEvents(context.Background(), 42, []api.FederationIngestEventEnvelope{{
		EventID: 1,
	}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned 409")
}

func TestFederationClientProjectFederation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/projects/42/federation/metadata", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(api.ProjectFederationBody{
			ProjectID:              42,
			ProjectUID:             "01HZNQ7VFPK1XGD8R5MABCD4EX",
			ProjectName:            "kata",
			ReplayHorizonEventID:   9,
			BaselineThroughEventID: 11,
		}))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(context.Background(), srv.URL, "", clientpkg.Opts{})
	require.NoError(t, err)
	body, err := client.ProjectFederation(context.Background(), 42)
	require.NoError(t, err)

	assert.Equal(t, int64(42), body.ProjectID)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", body.ProjectUID)
	assert.Equal(t, int64(9), body.ReplayHorizonEventID)
	assert.Equal(t, int64(11), body.BaselineThroughEventID)
}

func TestFederationClientPostJSONRejectsNonJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(context.Background(), srv.URL, "hub-token", clientpkg.Opts{})
	require.NoError(t, err)
	_, err = client.IngestProjectEvents(context.Background(), 42, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode hub")
}
