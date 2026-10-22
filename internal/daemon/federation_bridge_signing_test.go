package daemon_test

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
)

const bridgeSigningHubBase = "https://hub.example"

// signingBridgeFixture is a real hub daemon that requires request signing,
// verified by a real replay-protected verifier, plus a local daemon whose
// catalog selects that hub. Both run on in-memory listeners so the synctest
// bubble can advance through the verifier's startup quarantine.
type signingBridgeFixture struct {
	hubStore   db.Storage
	hubProject db.Project
	local      projectAccessFixture
	localStore db.Storage
	source     signingpolicy.Source
}

func newSigningBridgeFixture(t *testing.T) signingBridgeFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("TEST_BRIDGE_SIGNING_KEY", strings.Repeat("b", 64))
	source := signingpolicy.Source{KeyID: "bridge-key", KeyEnv: "TEST_BRIDGE_SIGNING_KEY"}
	state := filepath.Join(home, "replay.state")
	require.NoError(t, federationsigning.InitializeReplayState(state))
	hubStore := openReplicaServiceStore(t)
	hubProject, err := hubStore.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = hubStore.EnableProjectFederation(t.Context(), hubProject.ID, "admin")
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, hubStore.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: hubProject.UID, AuthorityUID: hubStore.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	rootSigner := db.RootAttributionSigner{AuthorityUID: hubStore.InstanceUID(), PrivateKey: private}
	_, _, err = hubStore.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "member-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	// A fresh hub store issues the bridge's grant as enrollment 1; the hub
	// operator registers the bridge key by that ID, as the signing guide says.
	verifier, err := federationsigning.NewVerifier(bridgeSigningHubBase, []signingpolicy.Key{{Source: source, EnrollmentID: 1}}, state)
	require.NoError(t, err)
	t.Cleanup(func() { _ = verifier.Close() })
	hubServer := daemon.NewServer(daemon.ServerConfig{DB: hubStore, RootSigning: daemon.NewRootSigning(rootSigner), FederationSigning: verifier, FederationSigningRequired: true, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, hubServer.Close()) })
	hubHTTP := httptest.NewTestServer(t, hubServer.Handler())
	previous := http.DefaultTransport
	http.DefaultTransport = hubHTTP.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	catalog := []config.CatalogDaemonConfig{{Name: "selected-hub", URL: bridgeSigningHubBase, InstanceUID: hubStore.InstanceUID(), Token: "member-test-token"}}
	localStore := openReplicaServiceStore(t)
	localServer := daemon.NewServer(daemon.ServerConfig{DB: localStore, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCredentials: config.DefaultFederationCredentialStore(), FederationCatalog: catalog})
	t.Cleanup(func() { require.NoError(t, localServer.Close()) })
	localHTTP := httptest.NewTestServer(t, localServer.Handler())
	// The in-memory test server has no address; its client reaches it by any name.
	localHTTP.URL = "http://local.example"
	return signingBridgeFixture{hubStore: hubStore, hubProject: hubProject, local: projectAccessFixture{store: localStore, server: localHTTP}, localStore: localStore, source: source}
}

func (f signingBridgeFixture) bridgeState(t *testing.T) string {
	t.Helper()
	code, _, raw := f.local.request(t, http.MethodGet, "/api/v1/federation/bridges/shared-replica", "", nil, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	var status map[string]any
	require.NoError(t, json.Unmarshal(raw, &status))
	state, _ := status["state"].(string)
	return state
}

// A hub that requires request signing refuses an unsigned bridge before any
// credential is saved, and names the command that configures signing.
func TestFederationBridgeConnectRefusesUnsignedBridgeToSigningHub(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSigningBridgeFixture(t)
		for _, preflight := range []bool{true, false} {
			code, _, raw := f.local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(f.hubProject.Name, preflight), bridgeOwnerHeaders)
			require.Equal(t, http.StatusConflict, code, "preflight=%v: %s", preflight, raw)
			var failure struct {
				Error struct {
					Code string `json:"code"`
					Hint string `json:"hint"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(raw, &failure))
			require.Equal(t, "hub_signing_required", failure.Error.Code)
			require.Contains(t, failure.Error.Hint, "kata federation signing configure --project-uid "+f.hubProject.UID)
			require.Contains(t, failure.Error.Hint, "--signing-key-id")
		}
		_, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), f.hubProject.UID)
		require.NoError(t, err)
		require.False(t, found, "a refused connect leaves no credential behind")
		grants, err := f.hubStore.ListFederationEnrollments(t.Context())
		require.NoError(t, err)
		require.Empty(t, grants)
	})
}

// A bridge connected with a signing source relays through a required-signing
// hub. An unsigned request is a signing problem, not a revoked grant: it is
// reported as such and signed sync resumes without reconnecting.
func TestFederationBridgeSignedRelayAndSignatureRejection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSigningBridgeFixture(t)
		body := bridgeConnectBody(f.hubProject.Name, false)
		body["signing_key_id"] = f.source.KeyID
		body["signing_key_env"] = f.source.KeyEnv
		code, _, raw := f.local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, bridgeOwnerHeaders)
		require.Equal(t, http.StatusOK, code, string(raw))
		grants, err := f.hubStore.ListFederationEnrollments(t.Context())
		require.NoError(t, err)
		require.Len(t, grants, 1)
		require.EqualValues(t, 1, grants[0].ID, "the verifier key is registered for enrollment 1")
		credential, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), f.hubProject.UID)
		require.NoError(t, err)
		require.True(t, found)
		require.NotNil(t, credential.Signing)
		require.Equal(t, f.source.KeyID, credential.Signing.KeyID)
		project, err := f.localStore.ProjectByUID(t.Context(), f.hubProject.UID)
		require.NoError(t, err)
		binding, err := f.localStore.FederationBindingByProject(t.Context(), project.ID)
		require.NoError(t, err)
		require.Equal(t, "connected", f.bridgeState(t))

		time.Sleep(federationsigning.Quarantine)
		unsigned := credential
		unsigned.Signing = nil
		err = federation.SyncFederationOnce(t.Context(), f.localStore, binding, unsigned)
		require.ErrorIs(t, err, federationsigning.ErrHubRejected)
		binding, err = f.localStore.FederationBindingByProject(t.Context(), project.ID)
		require.NoError(t, err)
		require.False(t, binding.RelayConfig.UpstreamRevoked, "a signature rejection is not a revoked grant")
		require.Equal(t, "signing_rejected", f.bridgeState(t))

		local, _, err := f.localStore.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Signed relay delivery", Author: "local-member"})
		require.NoError(t, err)
		require.NoError(t, federation.SyncFederationOnce(t.Context(), f.localStore, binding, credential))
		require.Equal(t, "connected", f.bridgeState(t))
		mirrored, err := f.hubStore.IssueByUID(t.Context(), local.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, "Signed relay delivery", mirrored.Title)
	})
}
