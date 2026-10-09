package daemon_test

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R9: the selected bridge's retained enrollment state is visible offline,
// including before project creation, without exposing its narrow credential.
func TestFederationBridgePendingStatus(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		t.Setenv("KATA_HOME", t.TempDir())
		const rootProjectUID = "00000000000000000000000003"
		credential := config.FederationCredential{HubURL: "https://hub.example", HubProjectID: 7, Token: "narrow-private-test-token", Capabilities: "claim,pull,push", Actor: "member", HubCatalog: "example-hub", RequestedActor: "local-member", SpokeProjectName: "shared-replica", RelayEnrollmentPending: true}
		require.NoError(t, config.WriteFederationCredential(rootProjectUID, credential))
		require.NoError(t, config.WriteFederationCredential("00000000000000000000000004", config.FederationCredential{HubURL: "https://unrelated.example", HubProjectID: 8, Token: "unrelated-private-test-token", SpokeProjectName: "unrelated-project"}))
		//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "local-member-test-token", Actor: "local-member", AdminActor: "admin"})
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		endpoint := httptest.NewServer(server.Handler())
		t.Cleanup(endpoint.Close)
		request := projectAccessFixture{store: store, server: endpoint}
		path := "/api/v1/federation/bridges/shared-replica"
		code, _, raw := request.request(t, http.MethodGet, path, "", nil, map[string]string{"Authorization": "Bearer local-owner-test-token"})
		require.Equal(t, http.StatusOK, code, string(raw))
		var status map[string]any
		require.NoError(t, json.Unmarshal(raw, &status))
		require.Equal(t, "enrollment_pending", status["state"])
		require.Equal(t, "bidirectional", status["direction"])
		require.Equal(t, "member", status["upstream_account"])
		require.Equal(t, "local-member", status["local_account"])
		require.Equal(t, rootProjectUID, status["project_uid"])
		require.NotContains(t, string(raw), credential.Token)
		require.NotContains(t, string(raw), "unrelated-private-test-token")
		require.NotContains(t, string(raw), "unrelated.example")
		_, err = store.ProjectByUID(t.Context(), rootProjectUID)
		require.ErrorIs(t, err, db.ErrNotFound)
		code, _, raw = request.request(t, http.MethodGet, path, "", nil, map[string]string{"Authorization": "Bearer local-member-test-token"})
		require.Equal(t, http.StatusNotFound, code, string(raw))
		require.NotContains(t, string(raw), rootProjectUID)
		code, _, raw = request.request(t, http.MethodGet, "/api/v1/federation/bridges/unknown-project", "", nil, map[string]string{"Authorization": "Bearer local-owner-test-token"})
		require.Equal(t, http.StatusNotFound, code, string(raw))
		require.NotContains(t, string(raw), "unrelated.example")
		// An ambiguous pending name is a conflict; status cannot choose one UID.
		duplicate := credential
		duplicate.HubURL = "https://other.example"
		require.NoError(t, config.WriteFederationCredential("00000000000000000000000005", duplicate))
		code, _, raw = request.request(t, http.MethodGet, path, "", nil, map[string]string{"Authorization": "Bearer local-owner-test-token"})
		require.Equal(t, http.StatusConflict, code, fmt.Sprint(string(raw)))
	})
}

func TestFederationBridgeStatusReportsPendingEnrollmentForPartialProject(t *testing.T) {
	for _, test := range []struct {
		name           string
		binding        bool
		credentialName string
		wantStatus     int
	}{
		{name: "project only", credentialName: "shared-replica", wantStatus: http.StatusOK},
		{name: "binding before relay config", binding: true, credentialName: "shared-replica", wantStatus: http.StatusOK},
		{name: "unmatched pending credential", binding: true, credentialName: "other-project", wantStatus: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				t.Setenv("KATA_HOME", t.TempDir())
				const projectUID = "00000000000000000000000006"
				project, err := store.CreateProjectWithUID(t.Context(), "shared-replica", projectUID)
				require.NoError(t, err)
				if test.binding {
					_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{
						ProjectID: project.ID, Role: db.FederationRoleSpoke,
						HubURL: "https://hub.example", HubProjectID: 7,
						HubProjectUID: "00000000000000000000000007",
						Actor:         "upstream-member", Enabled: true,
					})
					require.NoError(t, err)
				}
				credential := config.FederationCredential{
					HubURL: "https://hub.example", HubProjectID: 7,
					Token: "pending-bridge-test-token", Capabilities: "claim,pull,push",
					Actor: "upstream-member", HubCatalog: "example-hub",
					RequestedActor: "local-member", SpokeProjectName: test.credentialName,
					RelayEnrollmentPending: true,
				}
				require.NoError(t, config.DefaultFederationCredentialStore().StoreFederationCredential(
					t.Context(), project.UID, credential,
				))
				server := daemon.NewServer(daemon.ServerConfig{
					DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token"},
				})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				endpoint := httptest.NewServer(server.Handler())
				t.Cleanup(endpoint.Close)
				request := projectAccessFixture{store: store, server: endpoint}
				code, _, raw := request.request(t, http.MethodGet,
					"/api/v1/federation/bridges/shared-replica", "", nil,
					map[string]string{"Authorization": "Bearer local-owner-test-token"})
				require.Equal(t, test.wantStatus, code, string(raw))
				if test.wantStatus == http.StatusOK {
					var status map[string]any
					require.NoError(t, json.Unmarshal(raw, &status))
					require.Equal(t, "enrollment_pending", status["state"])
					require.Equal(t, project.UID, status["project_uid"])
					require.Equal(t, "local-member", status["local_account"])
					require.Equal(t, "upstream-member", status["upstream_account"])
					require.NotContains(t, string(raw), credential.Token)
				}
			})
		})
	}
}

// R9: active status derives its accounts and observed connection state from
// the existing binding/credential/sync diagnostics, with no network request.
func TestFederationBridgeActiveStatus(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		credentials := newReplicaCredentialStore()
		params := relayReplicaParams(t, store)
		params.Credential.HubCatalog = "example-hub"
		result, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCredentials: credentials})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		endpoint := httptest.NewServer(server.Handler())
		t.Cleanup(endpoint.Close)
		request := projectAccessFixture{store: store, server: endpoint}
		read := func(want string) {
			code, _, raw := request.request(t, http.MethodGet, "/api/v1/federation/bridges/"+result.Project.Name, "", nil, map[string]string{"Authorization": "Bearer local-owner-test-token"})
			require.Equal(t, http.StatusOK, code, string(raw))
			var status map[string]any
			require.NoError(t, json.Unmarshal(raw, &status))
			require.Equal(t, want, status["state"])
			require.Equal(t, "bidirectional", status["direction"])
			require.Equal(t, "local-member", status["local_account"])
			require.Equal(t, params.Credential.Actor, status["upstream_account"])
			require.Equal(t, result.Project.UID, status["project_uid"])
			require.NotContains(t, string(raw), params.Credential.Token)
			require.NotNil(t, status["relay"])
			require.NotNil(t, status["federation"])
		}
		read("connected")
		at := time.Now().UTC()
		require.NoError(t, store.RecordFederationSyncError(t.Context(), result.Project.ID, errors.New("upstream unavailable"), at))
		read("offline")
		require.NoError(t, store.RecordFederationSyncPullSuccess(t.Context(), result.Project.ID, at.Add(time.Second)))
		read("connected")
		binding, err := store.FederationBindingByProject(t.Context(), result.Project.ID)
		require.NoError(t, err)
		binding.Enabled = false
		_, err = store.UpsertFederationBinding(t.Context(), binding)
		require.NoError(t, err)
		read("paused")
		binding.Enabled = true
		_, err = store.UpsertFederationBinding(t.Context(), binding)
		require.NoError(t, err)
		relay := *binding.RelayConfig
		relay.UpstreamRevoked = true
		_, err = store.SetRelayBindingConfig(t.Context(), result.Project.ID, relay)
		require.NoError(t, err)
		read("revoked")
	})
}

func TestFederationStatusCountsRelayDescendants(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		t.Setenv("KATA_HOME", t.TempDir())
		credentials := newReplicaCredentialStore()
		params := relayReplicaParams(t, store)
		params.Credential.HubCatalog = "example-hub"
		result, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
		require.NoError(t, err)

		parent, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
			PlaintextToken: "relay-descendant-parent-test-token",
			Actor:          "local-member",
			AdminActor:     "admin",
		})
		require.NoError(t, err)
		_, err = store.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{
			ProjectID:        result.Project.ID,
			ParentTokenID:    parent.ID,
			SpokeInstanceUID: federationTestSpokeUID,
			ProtocolVersion:  db.RelayProtocolVersion,
			Token:            "relay-descendant-test-token",
			ServeDownstream:  false,
		})
		require.NoError(t, err)

		server := daemon.NewServer(daemon.ServerConfig{
			DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token"},
			FederationCredentials: credentials,
		})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		endpoint := httptest.NewServer(server.Handler())
		t.Cleanup(endpoint.Close)
		request := projectAccessFixture{store: store, server: endpoint}
		headers := map[string]string{"Authorization": "Bearer local-owner-test-token"}

		code, _, raw := request.request(t, http.MethodGet,
			projectPath(result.Project.ID)+"/federation/status", "", nil, headers)
		require.Equal(t, http.StatusOK, code, string(raw))
		var federationStatus api.FederationStatusBody
		require.NoError(t, json.Unmarshal(raw, &federationStatus))
		require.Len(t, federationStatus.Statuses, 1)
		assert.Equal(t, int64(1), federationStatus.Statuses[0].EnrollmentCount,
			"a relay that serves descendants reports its active child enrollment")

		code, _, raw = request.request(t, http.MethodGet,
			"/api/v1/federation/bridges/"+result.Project.Name, "", nil, headers)
		require.Equal(t, http.StatusOK, code, string(raw))
		var bridgeStatus api.FederationBridgeStatusBody
		require.NoError(t, json.Unmarshal(raw, &bridgeStatus))
		require.NotNil(t, bridgeStatus.Federation)
		assert.Equal(t, int64(1), bridgeStatus.Federation.EnrollmentCount,
			"bridge status reports the same downstream enrollment count")
	})
}
