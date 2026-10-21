package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// Enrollment and its exact retry must advertise the signed recovery path when
// a root checkpoint retained a mutation before the first portable snapshot.
func TestRelayDownstreamLateSnapshotHandshake(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	issue, creation, err := hub.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: hub.root.private.ID, Title: "Historical boundary issue", Author: "source-agent", Links: []db.InitialLink{{ToNumber: hub.root.visible.ID, Type: "related"}}})
	require.NoError(t, err)
	links, err := hub.store.LinksByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.NoError(t, hub.store.DeleteLinkByID(t.Context(), links[0].ID))
	assigned, err := hub.store.ClaimOwner(t.Context(), db.ClaimOwnerParams{IssueID: issue.ID, Actor: "source-agent"})
	require.NoError(t, err)
	store := openReplicaServiceStore(t)
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: "local-member", AdminActor: "admin", PlaintextToken: "connector-account-test-token"})
	require.NoError(t, err)
	credentials := newReplicaCredentialStore()
	server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token", RequireTokenIdentity: true}, FederationCredentials: credentials, FederationCatalog: hub.catalog(hub.root.server.URL)})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	local := projectAccessFixture{store: store, server: endpoint}
	body := bridgeConnectBody(hub.root.private.Name, false)
	body["serve_downstream"] = true
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	project, err := store.ProjectByUID(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	binding, err := store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	credential, found, err := credentials.FederationCredential(t.Context(), project.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, federation.SyncFederationOnce(t.Context(), store, binding, credential))
	history, err := store.EventsByUIDs(t.Context(), project.ID, []string{assigned.Event.UID})
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, assigned.Event.UID, history[0].UID)
	_, err = store.EventsByUIDs(t.Context(), project.ID, []string{creation.UID})
	require.ErrorIs(t, err, db.ErrNotFound)
	enroll := map[string]any{"spoke_instance_uid": "00000000000000000000000009", "project_id": project.ID, "capabilities": "claim,pull,push", "token": project.UID, "relay": map[string]any{"protocol_version": 1, "serve_downstream": false}}
	headers := map[string]string{"Authorization": "Bearer connector-account-test-token"}
	var original api.FederationEnrollmentOut
	for attempt := range 2 {
		code, _, raw = local.request(t, http.MethodPost, "/api/v1/federation/enrollments", "", enroll, headers)
		require.Equal(t, http.StatusOK, code, string(raw))
		var grant api.FederationEnrollmentOut
		require.NoError(t, json.Unmarshal(raw, &grant))
		require.NotNil(t, grant.Relay)
		require.True(t, grant.Relay.ResetRequired, "late introductions must advertise checkpoint recovery")
		if attempt == 0 {
			original = grant
		} else {
			require.Equal(t, original.Relay.BindingUID, grant.Relay.BindingUID, "retry retains the existing enrollment")
		}
		code, _, raw = local.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation/relay/reset", project.ID), "", nil, map[string]string{"Authorization": "Bearer " + project.UID})
		require.Equal(t, http.StatusOK, code, string(raw))
		var checkpoint db.RelayResetCheckpoint
		require.NoError(t, json.Unmarshal(raw, &checkpoint))
		pin, err := store.RootAuthority(t.Context(), project.UID)
		require.NoError(t, err)
		require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	}
}

// On a relay, only the account that connected the bridge can enroll a
// downstream daemon that will itself serve the project onward. Any member may
// still enroll a leaf.
func TestRelayDownstreamEnrollmentRequiresBridgeConnector(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	store := openReplicaServiceStore(t)
	for _, actor := range []string{"local-member", "other-member"} {
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: actor, AdminActor: "admin", PlaintextToken: actor + "-test-token"})
		require.NoError(t, err)
	}
	server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token", RequireTokenIdentity: true}, FederationCredentials: config.DefaultFederationCredentialStore(), FederationCatalog: hub.catalog(hub.root.server.URL)})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	local := projectAccessFixture{store: store, server: endpoint}
	body := bridgeConnectBody(hub.root.private.Name, false)
	body["serve_downstream"] = true
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	project, err := store.ProjectByUID(t.Context(), hub.root.private.UID)
	require.NoError(t, err)

	enroll := func(actor, token, peer string, serveDownstream bool) (int, []byte) {
		t.Helper()
		request := map[string]any{"spoke_instance_uid": peer, "project_id": project.ID, "capabilities": "claim,pull,push", "token": token, "relay": map[string]any{"protocol_version": 1, "serve_downstream": serveDownstream}}
		code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/enrollments", actor, request, nil)
		return code, raw
	}
	//nolint:gosec // These are deterministic fixture credentials, never operational secrets.
	code, raw = enroll("other-member", "other-downstream-test-token", "00000000000000000000000006", true)
	require.Equal(t, http.StatusForbidden, code, string(raw))
	require.Equal(t, "relay_downstream_forbidden", bridgeErrorCode(t, raw))
	_, err = store.AuthorizeFederationToken(t.Context(), "other-downstream-test-token", project.ID, "pull")
	require.ErrorIs(t, err, db.ErrNotFound, "a refused downstream enrollment issues no grant")

	code, raw = enroll("other-member", "other-leaf-test-token", "00000000000000000000000006", false)
	require.Equal(t, http.StatusOK, code, string(raw))
	code, raw = enroll("local-member", "connector-downstream-test-token", "00000000000000000000000007", true)
	require.Equal(t, http.StatusOK, code, string(raw))
	grants, err := store.ListFederationEnrollments(t.Context())
	require.NoError(t, err)
	require.Len(t, grants, 2)
	for _, grant := range grants {
		require.Equal(t, grant.Actor == "local-member", grant.RelayServeDownstream, "only the connector's grant serves downstream")
	}
}
