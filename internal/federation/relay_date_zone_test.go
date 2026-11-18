package federation_test

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

func TestRelayOfferRejectsZonedDatesWithoutFeature(t *testing.T) {
	root := newRelayMatrixNode(t, "sqlite", "root-member")
	leaf := newRelayMatrixNode(t, "sqlite", "leaf-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	enrollRelayMatrixReplica(t, root, leaf, "leaf-project", false)
	syncRelayMatrixNode(t, leaf)
	_, _, err = root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Scheduled", Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2027-05-12T09:00[Europe/Paris]"`)}})
	require.NoError(t, err)
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/federation/relay?stream=events&limit=100&epoch=1", root.http.URL, project.ID)
	offer := func(features string) (int, []byte) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+leaf.credential.Token)
		req.Header.Set(db.EventFeaturesHeader, features)
		resp, err := root.http.Client().Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, raw
	}
	state := relayState(t, root.store)
	code, raw := offer("")
	require.Equal(t, http.StatusConflict, code, string(raw))
	require.Contains(t, string(raw), "unsupported_event_features")
	require.Equal(t, state, relayState(t, root.store))
	code, raw = offer(db.SupportedEventFeatures)
	require.Equal(t, http.StatusOK, code, string(raw))
	var batch db.RelayBatch
	require.NoError(t, json.Unmarshal(raw, &batch))
	require.Len(t, batch.Envelopes, 1)
	last := batch.Envelopes[0]
	body, err := json.Marshal(map[string]any{"stream": "events", "epoch": 1, "through": last.Sequence, "digest": last.Digest})
	require.NoError(t, err)
	ack := func(features string) (int, []byte) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/federation/relay:ack", root.http.URL, project.ID), bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+leaf.credential.Token)
		req.Header.Set(db.EventFeaturesHeader, features)
		resp, err := root.http.Client().Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, raw
	}
	state = relayState(t, root.store)
	code, raw = ack("")
	require.Equal(t, http.StatusConflict, code, string(raw))
	require.Equal(t, state, relayState(t, root.store))
	code, raw = ack(db.SupportedEventFeatures)
	require.Equal(t, http.StatusOK, code, string(raw))
	_, _, err = root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Unzoned"})
	require.NoError(t, err)
	code, raw = offer("")
	require.Equal(t, http.StatusOK, code, string(raw))
}

func TestRelayResetRejectsZonedDatesWithoutChangingState(t *testing.T) {
	root := newRelayMatrixNode(t, "sqlite", "root-member")
	leaf := newRelayMatrixNode(t, "sqlite", "leaf-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	enrollRelayMatrixReplica(t, root, leaf, "leaf-project", false)
	syncRelayMatrixNode(t, leaf)
	_, _, err = root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "member", Title: "Scheduled", Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2027-05-12[Europe/Paris]"`)}})
	require.NoError(t, err)
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/federation/relay/reset", root.http.URL, project.ID)
	reset := func(features string) (int, []byte) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+leaf.credential.Token)
		req.Header.Set(db.EventFeaturesHeader, features)
		resp, err := root.http.Client().Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, raw
	}
	before, err := root.store.MaxEventID(t.Context())
	require.NoError(t, err)
	state := relayState(t, root.store)
	code, raw := reset("")
	require.Equal(t, http.StatusConflict, code, string(raw))
	after, err := root.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, state, relayState(t, root.store))
	code, raw = reset(db.SupportedEventFeatures)
	require.Equal(t, http.StatusOK, code, string(raw))
}

func relayState(t *testing.T, store db.Storage) []db.ImportRecord {
	t.Helper()
	var result []db.ImportRecord
	for record, err := range store.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		result = append(result, record)
	}
	return result
}

func TestRelayDoesNotEmitZonedDatesToOlderHub(t *testing.T) {
	root := newRelayMatrixNode(t, "sqlite", "root-member")
	leaf := newRelayMatrixNode(t, "sqlite", "leaf-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	enrollRelayMatrixReplica(t, root, leaf, "leaf-project", false)
	syncRelayMatrixNode(t, leaf)
	_, _, err = leaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: leaf.project.ID, Author: "member", Title: "Scheduled", Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2027-05-12[Europe/Paris]"`)}})
	require.NoError(t, err)
	original := *root.handler.Load()
	oldHub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		original.ServeHTTP(recorder, r)
		for key, values := range recorder.Header() {
			if !strings.EqualFold(key, db.EventFeaturesHeader) {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})
	var handler http.Handler = oldHub
	root.handler.Store(&handler)
	binding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
	require.NoError(t, err)
	before := relayState(t, leaf.store)
	maxID, err := root.store.MaxEventID(t.Context())
	require.NoError(t, err)
	err = federation.SyncFederationOnce(t.Context(), leaf.store, binding, leaf.credential)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	require.Equal(t, before, relayState(t, leaf.store))
	after, err := root.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, maxID, after)
}
