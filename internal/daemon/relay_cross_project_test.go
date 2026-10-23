package daemon_test

import (
	"context"
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
		other, err := store.CreateProject(ctx, "other-project")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "Other issue", Author: "admin"})
		require.NoError(t, err)
		_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: f.issue.ID, Type: "parent", Author: "admin"})
		require.NoError(t, err)
		_, private, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, RootSigning: daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		endpoint := httptest.NewServer(server.Handler())
		t.Cleanup(endpoint.Close)
		f.server = endpoint
		status, _, raw := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation?relay_protocol_version=1", f.private.ID), "member", nil, nil)
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Equal(t, "relay_cross_project_links", bridgeErrorCode(t, raw))
		// Ordinary federation metadata still supports legacy cross-project links.
		status, _, raw = f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation", f.private.ID), "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
	})
}

func TestFederationBridgeCrossProjectLinks(t *testing.T) {
	for _, phase := range []string{"metadata", "enrollment"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			hub := newBridgeHubFixture(t)
			other, err := hub.store.CreateProject(t.Context(), "other-project")
			require.NoError(t, err)
			issue, _, err := hub.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "Other issue", Author: "admin"})
			require.NoError(t, err)
			hubURL := hub.root.server.URL
			preflights := []bool{true, false}
			var linked atomic.Bool
			if phase == "metadata" {
				_, err = hub.store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: hub.root.issue.ID, ToIssueID: issue.ID, Type: "blocks", Author: "admin"})
				require.NoError(t, err)
			} else {
				// A link appearing after metadata must still prevent enrollment.
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
				hubURL = delayed.URL
				preflights = []bool{false}
			}
			local := newBridgeLocalDaemon(t, openReplicaServiceStore(t), hub.catalog(hubURL))
			for _, preflight := range preflights {
				code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, preflight), bridgeOwnerHeaders)
				require.Equal(t, http.StatusConflict, code, "preflight=%v: %s", preflight, raw)
				require.Equal(t, "relay_cross_project_links", bridgeErrorCode(t, raw))
			}
			if phase == "enrollment" {
				require.True(t, linked.Load())
			}
			grants, err := hub.store.ListFederationEnrollments(t.Context())
			require.NoError(t, err)
			require.Empty(t, grants)
			if phase == "metadata" {
				_, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
				require.NoError(t, err)
				require.False(t, found)
			}
		})
	}
}

func TestFederationBridgePreflightReportsUncheckedOldHub(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	oldHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Older hubs ignore the relay metadata query parameter.
		r.URL.RawQuery = ""
		hub.root.server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(oldHub.Close)
	local := newBridgeLocalDaemon(t, openReplicaServiceStore(t), hub.catalog(oldHub.URL))
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, true), bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var preview map[string]any
	require.NoError(t, json.Unmarshal(raw, &preview))
	require.Equal(t, "ready", preview["status"])
	require.Equal(t, false, preview["hub_links_checked"], "an older hub cannot confirm the link check")

	code, _, raw = local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, false), bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	require.Contains(t, string(raw), `"status":"connected"`)
}

func TestFederationBridgePreflightReportsCheckedHub(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	local := newBridgeLocalDaemon(t, openReplicaServiceStore(t), hub.catalog(hub.root.server.URL))
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, true), bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var preview map[string]any
	require.NoError(t, json.Unmarshal(raw, &preview))
	require.Equal(t, "ready", preview["status"])
	require.Equal(t, true, preview["hub_links_checked"])
}

// relayLinkCheckStore fails only the link check, as when the project is
// archived between the metadata read and the check.
type relayLinkCheckStore struct {
	db.Storage
	err error
}

func (s relayLinkCheckStore) CheckRelayProjectLinks(context.Context, int64) error { return s.err }

func TestRelayCrossProjectLinksHTTPProjectGone(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(t.Context(), f.private.ID, "admin")
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: relayLinkCheckStore{Storage: store, err: db.ErrNotFound}, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		endpoint := httptest.NewServer(server.Handler())
		t.Cleanup(endpoint.Close)
		f.server = endpoint
		status, _, raw := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation?relay_protocol_version=1", f.private.ID), "member", nil, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		require.Equal(t, "project_not_found", bridgeErrorCode(t, raw), "a vanished project is not a federation credential failure")
	})
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
			require.Contains(t, string(raw), "locally")
		})
	}
	grants, err := hub.store.ListFederationEnrollments(t.Context())
	require.NoError(t, err)
	require.Empty(t, grants)
	_, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), project.UID)
	require.NoError(t, err)
	require.False(t, found)
}

func TestFederationBridgeLocalProjectGoneDuringLinkCheck(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	store := openReplicaServiceStore(t)
	_, err := store.CreateProjectWithUID(t.Context(), "shared-replica", hub.root.private.UID)
	require.NoError(t, err)
	local := newBridgeLocalDaemon(t, relayLinkCheckStore{Storage: store, err: db.ErrNotFound}, hub.catalog(hub.root.server.URL))
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, true), bridgeOwnerHeaders)
	require.Equal(t, http.StatusNotFound, code, string(raw))
	require.Equal(t, "project_not_found", bridgeErrorCode(t, raw), "a local race must not look like a local authorization failure")
}
