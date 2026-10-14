package daemon_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// switchableHubProxy forwards to the hub until it is taken down, after which
// every connection is dropped as if the hub host no longer existed.
func switchableHubProxy(t *testing.T, hub bridgeHubFixture) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var down atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			connection, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		req := r.Clone(r.Context())
		req.RequestURI = ""
		req.URL.Scheme = "http"
		req.URL.Host = hub.root.server.Listener.Addr().String()
		req.Host = req.URL.Host
		// Let the forwarding client negotiate and decode compression itself, so
		// the body copied back is never a compressed body without its header.
		req.Header.Del("Accept-Encoding")
		//nolint:gosec // The destination is this test's httptest loopback listener.
		response, err := hub.root.server.Client().Do(req)
		if err != nil {
			http.Error(w, "fixture forwarding failed", http.StatusInternalServerError)
			return
		}
		defer func() { _ = response.Body.Close() }()
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(proxy.Close)
	return proxy, &down
}

type bridgeDisconnectBody struct {
	ProjectName string `json:"project_name"`
	ProjectUID  string `json:"project_uid"`
	Status      string `json:"status"`
	LocalOnly   bool   `json:"local_only"`
	Warning     string `json:"warning"`
}

// A bridge whose hub is gone for good can be detached locally: the project
// and its undelivered issue stay, all bridge state goes, and the response warns
// that the hub grant was not revoked.
func TestFederationBridgeLocalOnlyDisconnectWithoutHub(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	proxy, down := switchableHubProxy(t, hub)
	store := openReplicaServiceStore(t)
	local := newBridgeLocalDaemon(t, store, hub.catalog(proxy.URL))
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, false), bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	project, err := store.ProjectByUID(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	binding, err := store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	bindingUID := binding.RelayConfig.BindingUID
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Written after the hub went away", Author: "local-member"})
	require.NoError(t, err)
	down.Store(true)

	// An ordinary disconnect waits for the hub to accept the retained issue,
	// which a hub that is gone never does.
	path := "/api/v1/federation/bridges/shared-replica/disconnect"
	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusConflict, code, string(raw))
	require.Equal(t, "federation_lifecycle_blocked", bridgeErrorCode(t, raw))
	undelivered := 0
	for record, err := range store.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		if outbox, ok := record.(*db.RelayOutboxExport); ok && outbox.BindingUID == bindingUID && !outbox.Acknowledged {
			undelivered++
		}
	}
	require.Positive(t, undelivered, "the local issue waits in the relay outbox")
	before, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, before.LeavePending, "a refused ordinary disconnect changes nothing")

	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{"preflight": true, "local_only": true}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var preview bridgeDisconnectBody
	require.NoError(t, json.Unmarshal(raw, &preview))
	require.Equal(t, "ready", preview.Status)
	require.True(t, preview.LocalOnly)
	require.Contains(t, preview.Warning, "not revoked")
	credential, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.True(t, found, "preflight changes nothing")
	require.True(t, before.Equal(credential))

	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{"local_only": true}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var detached bridgeDisconnectBody
	require.NoError(t, json.Unmarshal(raw, &detached))
	require.Equal(t, "disconnected", detached.Status)
	require.Equal(t, hub.root.private.UID, detached.ProjectUID)
	require.True(t, detached.LocalOnly)
	require.Contains(t, detached.Warning, "not revoked")
	require.NotContains(t, string(raw), credential.Token)

	_, found, err = config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.False(t, found)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	kept, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, project.ID, kept.ProjectID)
	for record, err := range store.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		switch value := record.(type) {
		case *db.RelayOutboxExport:
			require.NotEqual(t, bindingUID, value.BindingUID)
		case *db.RelayInboxExport:
			require.NotEqual(t, bindingUID, value.BindingUID)
		case *db.RelayCursorExport:
			require.NotEqual(t, bindingUID, value.BindingUID)
		}
	}
	grants, err := hub.store.ListFederationEnrollments(t.Context())
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Nil(t, grants[0].RevokedAt, "a local-only disconnect leaves the hub grant for its operator to revoke")
	code, _, raw = local.request(t, http.MethodGet, "/api/v1/federation/bridges/shared-replica", "", nil, bridgeOwnerHeaders)
	require.Equal(t, http.StatusNotFound, code, string(raw))

	// A retry after a lost success response still tells the operator that the
	// hub grant needs revoking.
	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{"local_only": true}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var again bridgeDisconnectBody
	require.NoError(t, json.Unmarshal(raw, &again))
	require.Equal(t, "disconnected", again.Status)
	require.True(t, again.LocalOnly, "a completed local-only detach reports its mode on retry")
	require.Contains(t, again.Warning, "not revoked")
}

// A database restored without its credential file still holds the bridge
// binding. A local-only detach resolves that binding and needs no saved hub
// credential; an ordinary disconnect still does.
func TestFederationBridgeLocalOnlyDisconnectWithoutCredential(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	proxy, down := switchableHubProxy(t, hub)
	store := openReplicaServiceStore(t)
	local := newBridgeLocalDaemon(t, store, hub.catalog(proxy.URL))
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, false), bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	project, err := store.ProjectByUID(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	binding, err := store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	bindingUID := binding.RelayConfig.BindingUID
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Written before the credential file was lost", Author: "local-member"})
	require.NoError(t, err)
	down.Store(true)
	require.NoError(t, config.DefaultFederationCredentialStore().DeleteFederationCredential(t.Context(), hub.root.private.UID))

	path := "/api/v1/federation/bridges/shared-replica/disconnect"
	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusConflict, code, string(raw))
	require.Equal(t, "federation_credential_conflict", bridgeErrorCode(t, raw), "an ordinary disconnect still needs the hub credential")

	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{"preflight": true, "local_only": true}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var preview bridgeDisconnectBody
	require.NoError(t, json.Unmarshal(raw, &preview))
	require.Equal(t, "ready", preview.Status)
	require.True(t, preview.LocalOnly)
	require.Contains(t, preview.Warning, "not revoked")
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err, "preflight changes nothing")

	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{"local_only": true}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var detached bridgeDisconnectBody
	require.NoError(t, json.Unmarshal(raw, &detached))
	require.Equal(t, "disconnected", detached.Status)
	require.Equal(t, hub.root.private.UID, detached.ProjectUID)
	require.True(t, detached.LocalOnly)
	require.Contains(t, detached.Warning, "not revoked")

	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	kept, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, project.ID, kept.ProjectID)
	for record, err := range store.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		if outbox, ok := record.(*db.RelayOutboxExport); ok {
			require.NotEqual(t, bindingUID, outbox.BindingUID, "the detached binding's relay state is discarded")
		}
	}
	_, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.False(t, found, "a detach without a credential saves none")
	grants, err := hub.store.ListFederationEnrollments(t.Context())
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Nil(t, grants[0].RevokedAt, "a local-only disconnect leaves the hub grant for its operator to revoke")

	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{"local_only": true}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var again bridgeDisconnectBody
	require.NoError(t, json.Unmarshal(raw, &again))
	require.Equal(t, "disconnected", again.Status)
	require.True(t, again.LocalOnly, "a completed detach without a credential still reports its mode on retry")
}

// A hub that vanished while enrollment was still pending leaves a spoke
// binding without relay configuration, which status reports as
// enrollment_pending. Once the credential entry is gone too, the owner can
// still detach that bridge locally from the binding alone.
func TestFederationBridgeLocalOnlyDisconnectPendingEnrollmentWithoutCredential(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	store := openReplicaServiceStore(t)
	project, err := store.CreateProject(t.Context(), "shared-replica")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Written before the hub vanished", Author: "local-member"})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 7,
		HubProjectUID: "00000000000000000000000007", Actor: "upstream-member", Enabled: true,
	})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	pending := config.FederationCredential{HubURL: "https://hub.example", HubProjectID: 7, Token: "pending-bridge-test-token", Capabilities: "claim,pull,push", Actor: "upstream-member", HubCatalog: "example-hub", RequestedActor: "local-member", SpokeProjectName: "shared-replica", RelayEnrollmentPending: true}
	require.NoError(t, config.DefaultFederationCredentialStore().StoreFederationCredential(t.Context(), project.UID, pending))
	local := newBridgeLocalDaemon(t, store, nil)
	code, _, raw := local.request(t, http.MethodGet, "/api/v1/federation/bridges/shared-replica", "", nil, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var status map[string]any
	require.NoError(t, json.Unmarshal(raw, &status))
	require.Equal(t, "enrollment_pending", status["state"], "the fixture is the mid-enrollment state")
	require.NoError(t, config.DefaultFederationCredentialStore().DeleteFederationCredential(t.Context(), project.UID))

	path := "/api/v1/federation/bridges/shared-replica/disconnect"
	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{"preflight": true, "local_only": true}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var preview bridgeDisconnectBody
	require.NoError(t, json.Unmarshal(raw, &preview))
	require.Equal(t, "ready", preview.Status)
	require.True(t, preview.LocalOnly)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err, "preflight changes nothing")

	code, _, raw = local.request(t, http.MethodPost, path, "", map[string]any{"local_only": true}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var detached bridgeDisconnectBody
	require.NoError(t, json.Unmarshal(raw, &detached))
	require.Equal(t, "disconnected", detached.Status)
	require.Equal(t, project.UID, detached.ProjectUID)
	require.True(t, detached.LocalOnly)
	require.Contains(t, detached.Warning, "not revoked")
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	kept, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, project.ID, kept.ProjectID)
	code, _, raw = local.request(t, http.MethodGet, "/api/v1/federation/bridges/shared-replica", "", nil, bridgeOwnerHeaders)
	require.Equal(t, http.StatusNotFound, code, string(raw))
}

// Downstream relays depend on this bridge, so a local-only disconnect is
// refused, in preflight and for real, until they are revoked.
func TestFederationBridgeLocalOnlyDisconnectRefusesDownstream(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	store := openReplicaServiceStore(t)
	local := newBridgeLocalDaemon(t, store, hub.catalog(hub.root.server.URL))
	body := bridgeConnectBody(hub.root.private.Name, false)
	body["serve_downstream"] = true
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	project, err := store.ProjectByUID(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: "local-member", AdminActor: "admin", PlaintextToken: "leaf-parent-test-token"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	_, err = store.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "leaf-relay-test-token"})
	require.NoError(t, err)
	before, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.True(t, found)

	for _, preflight := range []bool{true, false} {
		code, _, raw = local.request(t, http.MethodPost, "/api/v1/federation/bridges/shared-replica/disconnect", "", map[string]any{"preflight": preflight, "local_only": true}, bridgeOwnerHeaders)
		require.Equal(t, http.StatusConflict, code, "preflight=%v: %s", preflight, raw)
		require.Equal(t, "federation_downstream_enrollments", bridgeErrorCode(t, raw))
	}
	after, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, before.Equal(after))
	binding, err := store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	require.True(t, binding.Enabled)
}

// Reconnecting after a local-only detach must not lose the edits the hub
// never saw: the new binding offers them to the hub before any reset, and
// they stay in the local project.
func TestFederationBridgeReconnectAfterLocalOnlyDeliversRetainedWork(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	proxy, down := switchableHubProxy(t, hub)
	store := openReplicaServiceStore(t)
	local := newBridgeLocalDaemon(t, store, hub.catalog(proxy.URL))
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, false), bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	project, err := store.ProjectByUID(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	first, err := store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	firstCredential, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, federation.SyncFederationOnce(t.Context(), store, first, firstCredential), "the first binding settles its bootstrap deliveries")
	down.Store(true)
	offline, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Written while the hub was gone", Author: "local-member"})
	require.NoError(t, err)
	code, _, raw = local.request(t, http.MethodPost, "/api/v1/federation/bridges/shared-replica/disconnect", "", map[string]any{"local_only": true}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	// While the project is detached the hub purges history the spoke holds,
	// so the reconnected binding must install a reset before it can follow.
	_, err = hub.store.PurgeIssue(t.Context(), hub.root.issue.ID, "admin", nil)
	require.NoError(t, err)
	detached, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Written while detached", Author: "local-member"})
	require.NoError(t, err)

	down.Store(false)
	for _, preflight := range []bool{true, false} {
		code, _, raw = local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, preflight), bridgeOwnerHeaders)
		require.Equal(t, http.StatusOK, code, "preflight=%v: %s", preflight, raw)
	}
	binding, err := store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	require.NotNil(t, binding.RelayConfig)
	credential, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, federation.SyncFederationOnce(t.Context(), store, binding, credential))
	after, err := store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	require.Greater(t, after.RelayConfig.ResetEpoch, binding.RelayConfig.ResetEpoch, "the reconnect installed the hub's reset")
	for _, issue := range []db.Issue{offline, detached} {
		delivered, err := hub.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err, "the hub receives %q after reconnect", issue.Title)
		require.Equal(t, issue.Title, delivered.Title)
		kept, err := store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err, "%q survives the reset", issue.Title)
		require.Equal(t, issue.Title, kept.Title)
	}
	_, err = store.IssueByUID(t.Context(), hub.root.issue.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound, "the reset removes the issue the hub purged")
}
