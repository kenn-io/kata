package daemon_test

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestRelayCrossProjectLinksHTTP(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		_, err := store.EnableProjectFederation(ctx, f.private.ID, "admin")
		require.NoError(t, err)
		// The member can see this project, but cannot see the other endpoint.
		other, err := store.CreateProject(ctx, "other-project")
		require.NoError(t, err)
		otherTeam, _, err := store.CreateTeam(ctx, "other-team", "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: other.UID, Visibility: "teams", TeamUIDs: []string{otherTeam.UID}}, "admin")
		require.NoError(t, err)
		hidden, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "Hidden endpoint", Author: "admin"})
		require.NoError(t, err)
		_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: hidden.ID, ToIssueID: f.issue.ID, Type: "parent", Author: "admin"})
		require.NoError(t, err)
		_, private, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, RootSigning: daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		endpoint := httptest.NewServer(server.Handler())
		t.Cleanup(endpoint.Close)
		f.server = endpoint
		body := map[string]any{"spoke_instance_uid": "00000000000000000000000006", "project_id": f.private.ID, "capabilities": "claim,pull,push", "relay": map[string]any{"protocol_version": 1}}
		for _, operation := range []string{"enrollment", "metadata"} {
			var status int
			var raw []byte
			if operation == "metadata" {
				status, _, raw = f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation?relay_protocol_version=1", f.private.ID), "member", nil, nil)
			} else {
				status, _, raw = f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
			}
			require.Equal(t, http.StatusConflict, status, "%s: %s", operation, raw)
			var failure struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Hint    string `json:"hint"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(raw, &failure))
			require.Equal(t, "relay_cross_project_links", failure.Error.Code)
			require.Contains(t, failure.Error.Message, "cross-project")
			require.Contains(t, failure.Error.Hint, "remove")
			require.NotContains(t, string(raw), other.Name)
			require.NotContains(t, string(raw), hidden.UID)
			require.NotContains(t, string(raw), hidden.ShortID)
		}
		grants, err := store.ListFederationEnrollments(ctx)
		require.NoError(t, err)
		require.Empty(t, grants)
		status, _, raw := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation?relay_protocol_version=1", f.private.ID), "nonmember", nil, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		require.NotContains(t, string(raw), "relay_cross_project_links")
		// Ordinary federation metadata still supports legacy cross-project links.
		status, _, raw = f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation", f.private.ID), "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
	})
}

func TestRelayMetadataArchivedProjectNotFoundHTTP(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		archived, err := store.CreateProject(t.Context(), "archived-project")
		require.NoError(t, err)
		_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: archived.ID, Actor: "admin"})
		require.NoError(t, err)
		for _, query := range []string{"", "?relay_protocol_version=1"} {
			status, _, raw := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation%s", archived.ID, query), "admin", nil, nil)
			assertAPIError(t, status, raw, http.StatusNotFound, "project_not_found")
		}
	})
}

func TestFederationBridgeCrossProjectLinks(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	other, err := hub.store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	issue, _, err := hub.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "Other issue", Author: "admin"})
	require.NoError(t, err)
	_, err = hub.store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: hub.root.issue.ID, ToIssueID: issue.ID, Type: "blocks", Author: "admin"})
	require.NoError(t, err)
	local := newBridgeLocalDaemon(t, openReplicaServiceStore(t), hub.catalog(hub.root.server.URL))
	for _, preflight := range []bool{true, false} {
		code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, preflight), bridgeOwnerHeaders)
		require.Equal(t, http.StatusConflict, code, "preflight=%v: %s", preflight, raw)
		require.Equal(t, "relay_cross_project_links", bridgeErrorCode(t, raw))
		require.Contains(t, string(raw), "remove")
		require.NotContains(t, string(raw), "check account access")
	}
	grants, err := hub.store.ListFederationEnrollments(t.Context())
	require.NoError(t, err)
	require.Empty(t, grants)
	_, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.False(t, found)
}

func TestFederationBridgePreflightRejectsOldHubLinkCheck(t *testing.T) {
	for _, version := range []string{"", "0.30.9"} {
		t.Run("api_schema_version="+version, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			hub := newBridgeHubFixture(t)
			var healthCalls atomic.Int32
			oldHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/api/v1/health" {
					healthCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					health := map[string]any{"ok": true}
					if version != "" {
						health["api_schema_version"] = version
					}
					_ = json.MarshalWrite(w, health)
					return
				}
				// Older hubs ignore the relay metadata query parameter.
				r.URL.RawQuery = ""
				hub.root.server.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(oldHub.Close)
			local := newBridgeLocalDaemon(t, openReplicaServiceStore(t), hub.catalog(oldHub.URL))
			code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, true), bridgeOwnerHeaders)
			require.Equal(t, http.StatusConflict, code, string(raw))
			require.Equal(t, "hub_link_check_unsupported", bridgeErrorCode(t, raw))
			require.Contains(t, string(raw), "cross-project links")
			require.Contains(t, string(raw), "upgrade the hub")
			require.Contains(t, string(raw), "upgrade the hub to API 0.31.0 or newer so preflight can check for cross-project links")
			require.EqualValues(t, 1, healthCalls.Load())

			code, _, raw = local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, false), bridgeOwnerHeaders)
			require.Equal(t, http.StatusOK, code, string(raw))
			require.Contains(t, string(raw), `"status":"connected"`)
			require.EqualValues(t, 1, healthCalls.Load(), "connect must not check the hub API schema version")
		})
	}
}

func TestFederationBridgeLocalCrossProjectLinks(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(t.Context(), "shared-replica", hub.root.private.UID)
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Local issue", Author: "admin"})
	require.NoError(t, err)
	other, err := store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	otherIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "Other issue", Author: "admin"})
	require.NoError(t, err)
	_, err = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: otherIssue.ID, Type: "blocks", Author: "admin"})
	require.NoError(t, err)
	local := newBridgeLocalDaemon(t, store, hub.catalog(hub.root.server.URL))
	for _, preflight := range []bool{true, false} {
		t.Run(fmt.Sprintf("preflight=%v", preflight), func(t *testing.T) {
			code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, preflight), bridgeOwnerHeaders)
			require.Equal(t, http.StatusConflict, code, string(raw))
			require.Equal(t, "relay_cross_project_links", bridgeErrorCode(t, raw))
			require.Contains(t, string(raw), `"message":"this daemon's copy of the project has cross-project issue links"`)
			require.Contains(t, string(raw), `"hint":"remove cross-project related, blocks, and parent links locally before retrying"`)
		})
	}
	grants, err := hub.store.ListFederationEnrollments(t.Context())
	require.NoError(t, err)
	require.Empty(t, grants)
	_, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), project.UID)
	require.NoError(t, err)
	require.False(t, found)
}

// A link may appear after metadata was read. The enrollment-time conflict
// still keeps its actionable meaning across the downstream daemon.
func TestFederationBridgeCrossProjectLinksAtEnrollment(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	other, err := hub.store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	issue, _, err := hub.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "Other issue", Author: "admin"})
	require.NoError(t, err)
	var linked atomic.Bool
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/federation/enrollments" {
			_, linkErr := hub.store.CreateLink(r.Context(), db.CreateLinkParams{FromIssueID: hub.root.issue.ID, ToIssueID: issue.ID, Type: "related", Author: "admin"})
			if linkErr != nil {
				t.Error(linkErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			linked.Store(true)
		}
		hub.root.server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(delayed.Close)
	local := newBridgeLocalDaemon(t, openReplicaServiceStore(t), hub.catalog(delayed.URL))
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, false), bridgeOwnerHeaders)
	require.True(t, linked.Load())
	require.Equal(t, http.StatusConflict, code, string(raw))
	require.Equal(t, "relay_cross_project_links", bridgeErrorCode(t, raw))
	require.Contains(t, string(raw), "remove")
	require.NotContains(t, string(raw), "check account access")
	grants, err := hub.store.ListFederationEnrollments(t.Context())
	require.NoError(t, err)
	require.Empty(t, grants)
}
