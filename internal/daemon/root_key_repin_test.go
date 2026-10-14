package daemon_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federationcoord"
)

func encodeRootKeySeed(key ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key.Seed()) + "\n"
}

type rootRepinFixture struct {
	store      db.Storage
	server     *httptest.Server
	hub        db.Project
	standalone db.Project
	oldPin     db.RootKeyPin
	oldKey     ed25519.PrivateKey
	keyFile    string
}

// newRootRepinFixture starts an owner-local server whose hub project is pinned
// to a key the daemon does not hold. keyFile seeds federation-root.key.
func newRootRepinFixture(t *testing.T, store db.Storage, keyFile ed25519.PrivateKey) rootRepinFixture {
	t.Helper()
	ctx := t.Context()
	f := rootRepinFixture{store: store, keyFile: filepath.Join(t.TempDir(), "federation-root.key")}
	var err error
	f.hub, err = store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.hub.ID, Role: db.FederationRoleHub, HubProjectID: f.hub.ID, HubProjectUID: f.hub.UID, Enabled: true})
	require.NoError(t, err)
	f.standalone, err = store.CreateProject(ctx, "standalone-project")
	require.NoError(t, err)
	oldPub, oldKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	f.oldKey = oldKey
	f.oldPin = db.RootKeyPin{ProjectUID: f.hub.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(oldPub), PublicKey: oldPub}
	require.NoError(t, store.PinRootAuthority(ctx, f.oldPin))
	if keyFile != nil {
		require.NoError(t, os.WriteFile(f.keyFile, []byte(encodeRootKeySeed(keyFile)), 0o600))
	}
	signing, err := daemon.LoadRootSigning(ctx, store, f.keyFile)
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: store, RootSigning: signing})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	f.server = httptest.NewServer(server.Handler())
	t.Cleanup(f.server.Close)
	return f
}

func (f rootRepinFixture) createIssue(t *testing.T, project db.Project) (int, map[string]any) {
	t.Helper()
	resp, raw := postJSON(t, f.server, fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), map[string]any{"actor": "local-owner", "title": "Signed task", "force_new": true})
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body), string(raw))
	return resp.StatusCode, body
}

func (f rootRepinFixture) repin(t *testing.T, body map[string]any) (int, api.FederationRootKeyRepinResult, string) {
	t.Helper()
	resp, raw := postJSON(t, f.server, "/api/v1/federation/root-keys/hub-project/repin", body)
	var result api.FederationRootKeyRepinResult
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(raw, &result), string(raw))
	}
	return resp.StatusCode, result, string(raw)
}

// enrollRelay grants a downstream relay on the fixture's hub project. The relay
// pins the hub's current key, so a later unsigned repin strands it.
func (f rootRepinFixture) enrollRelay(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	//nolint:gosec // These are deterministic fixture credentials, never operational secrets.
	parent, _, err := f.store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "repin-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	_, err = f.store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.hub.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "repin-relay-test-token"})
	require.NoError(t, err)
}

func (f rootRepinFixture) health(t *testing.T) *api.RootSigningHealth {
	t.Helper()
	var health struct {
		RootSigning *api.RootSigningHealth `json:"root_signing"`
	}
	require.NoError(t, json.Unmarshal([]byte(getBody(t, f.server, "/api/v1/health")), &health))
	return health.RootSigning
}

func (f rootRepinFixture) hubSigningStatus(t *testing.T) string {
	t.Helper()
	var status api.FederationStatusBody
	require.NoError(t, json.Unmarshal([]byte(getBody(t, f.server, "/api/v1/federation/status")), &status))
	for _, project := range status.Statuses {
		if project.ProjectUID == f.hub.UID {
			return project.RootSigningKey
		}
	}
	t.Fatalf("hub project missing from federation status")
	return ""
}

// A hub restored without federation-root.key starts degraded: signed writes
// to its pinned project fail with the recovery command, other projects keep
// working, and an explicit repin restores writes with a new key.
func TestRootKeyRepinAfterKeyLoss(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newRootRepinFixture(t, store, nil)
		status, body := f.createIssue(t, f.hub)
		require.Equal(t, http.StatusConflict, status, body)
		require.Equal(t, "root_signing_key_unavailable", body["error"].(map[string]any)["code"])
		require.Contains(t, body["error"].(map[string]any)["message"], "project hub-project")
		require.Contains(t, body["error"].(map[string]any)["message"], "kata federation root-key repin --project hub-project")
		status, body = f.createIssue(t, f.standalone)
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, &api.RootSigningHealth{KeyFile: f.keyFile, KeyLoaded: false, KeyProblem: "key file is missing", Projects: []string{"hub-project"}}, f.health(t))
		require.Equal(t, "unavailable", f.hubSigningStatus(t))

		code, preview, raw := f.repin(t, map[string]any{"preflight": true})
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "ready", preview.Status)
		require.Equal(t, "unsigned_repin", preview.Method)
		require.True(t, preview.CreatesKeyFile)
		require.Equal(t, f.oldPin.KeyID, preview.PreviousKeyID)
		require.Contains(t, preview.Consequence, "cannot follow an unsigned repin")
		require.NotContains(t, preview.Consequence, "enroll", "re-enrolling does not replace a downstream relay's pin")
		_, err := os.Stat(f.keyFile)
		require.ErrorIs(t, err, os.ErrNotExist, "preflight never creates a key")
		status, _ = f.createIssue(t, f.hub)
		require.Equal(t, http.StatusConflict, status, "preflight leaves the project unavailable")

		code, result, raw := f.repin(t, map[string]any{})
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "repinned", result.Status)
		require.Equal(t, "unsigned_repin", result.Method)
		require.FileExists(t, f.keyFile)
		pin, err := store.RootAuthority(t.Context(), f.hub.UID)
		require.NoError(t, err)
		require.Equal(t, result.KeyID, pin.KeyID)
		require.NotEqual(t, f.oldPin.KeyID, pin.KeyID)
		status, body = f.createIssue(t, f.hub)
		require.Equal(t, http.StatusOK, status, body)
		issueUID := body["issue"].(map[string]any)["uid"].(string)
		receipt, err := store.EntityAttribution(t.Context(), f.hub.UID, "issue", issueUID)
		require.NoError(t, err)
		require.NoError(t, db.VerifyRootReceipt(pin, receipt))
		require.Nil(t, f.health(t))
		require.Equal(t, "available", f.hubSigningStatus(t))
		_, err = federationcoord.RelayRootTransitionPath(f.oldPin, pin, nil)
		require.Error(t, err, "a downstream relay pinned to the lost key cannot follow an unsigned repin")

		code, again, raw := f.repin(t, map[string]any{})
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "current", again.Status)
	})
}

// A daemon that started without its key keeps serving once the operator
// restores the matching key file: repin reports the pin as current and loads
// the key, so signed writes succeed without a restart.
func TestRootKeyRepinLoadsRestoredKey(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newRootRepinFixture(t, store, nil)
		require.False(t, f.health(t).KeyLoaded)
		require.NoError(t, os.WriteFile(f.keyFile, []byte(encodeRootKeySeed(f.oldKey)), 0o600))

		code, preview, raw := f.repin(t, map[string]any{"preflight": true})
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "current", preview.Status)
		status, _ := f.createIssue(t, f.hub)
		require.Equal(t, http.StatusConflict, status, "preflight never loads the key")

		code, result, raw := f.repin(t, map[string]any{})
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "current", result.Status)
		require.Equal(t, f.oldPin.KeyID, result.KeyID)
		status, body := f.createIssue(t, f.hub)
		require.Equal(t, http.StatusOK, status, body)
		pin, err := store.RootAuthority(t.Context(), f.hub.UID)
		require.NoError(t, err)
		require.Equal(t, f.oldPin, pin, "a restored key changes no pin")
		require.Nil(t, f.health(t))
		require.Equal(t, "available", f.hubSigningStatus(t))
	})
}

// With the previous key available, repin is a signed rotation that a
// downstream relay pinned to the previous key accepts.
func TestRootKeyRepinSignedRotation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		_, loaded, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		f := newRootRepinFixture(t, store, loaded)
		f.enrollRelay(t)
		require.Equal(t, []string{"hub-project"}, f.health(t).Projects)
		status, _ := f.createIssue(t, f.hub)
		require.Equal(t, http.StatusConflict, status)

		downstream, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "downstream.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, downstream.Close()) })
		_, err = downstream.CreateProjectWithUID(t.Context(), "hub-project", f.hub.UID)
		require.NoError(t, err)
		require.NoError(t, downstream.PinRootAuthority(t.Context(), f.oldPin))

		code, _, raw := f.repin(t, map[string]any{"previous_key_file": "previous.key"})
		require.Equal(t, http.StatusBadRequest, code, raw)
		require.Contains(t, raw, "absolute path on the daemon host", "a client-relative path is never resolved against the daemon's working directory")

		wrong := filepath.Join(t.TempDir(), "wrong.key")
		_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(wrong, []byte(encodeRootKeySeed(wrongKey)), 0o600))
		code, _, raw = f.repin(t, map[string]any{"previous_key_file": wrong})
		require.Equal(t, http.StatusConflict, code, raw)
		require.Contains(t, raw, "root_signing_key_mismatch")

		previous := filepath.Join(t.TempDir(), "previous.key")
		require.NoError(t, os.WriteFile(previous, []byte(encodeRootKeySeed(f.oldKey)), 0o600))
		code, preview, raw := f.repin(t, map[string]any{"previous_key_file": previous, "preflight": true})
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "signed_rotation", preview.Method)
		require.False(t, preview.CreatesKeyFile)
		require.Equal(t, int64(1), preview.ActiveRelayEnrollments)
		require.NotContains(t, preview.Consequence, "--strand-relays", "a signed rotation strands nobody")
		code, result, raw := f.repin(t, map[string]any{"previous_key_file": previous})
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "rotated", result.Status)
		require.Equal(t, db.RootSignerKeyID(db.RootAttributionSigner{PrivateKey: loaded}), result.KeyID)

		pin, err := store.RootAuthority(t.Context(), f.hub.UID)
		require.NoError(t, err)
		history, err := store.RootKeyTransitions(t.Context(), f.hub.UID)
		require.NoError(t, err)
		path, err := federationcoord.RelayRootTransitionPath(f.oldPin, pin, history)
		require.NoError(t, err)
		for _, transition := range path {
			require.NoError(t, downstream.RotateRootAuthority(t.Context(), transition))
		}
		downstreamPin, err := downstream.RootAuthority(t.Context(), f.hub.UID)
		require.NoError(t, err)
		require.Equal(t, pin, downstreamPin)

		status, body := f.createIssue(t, f.hub)
		require.Equal(t, http.StatusOK, status, body)
		receipt, err := store.EntityAttribution(t.Context(), f.hub.UID, "issue", body["issue"].(map[string]any)["uid"].(string))
		require.NoError(t, err)
		require.NoError(t, db.VerifyRootReceipt(downstreamPin, receipt), "the downstream pin verifies receipts after rotation")
	})
}

// A fresh relay enrolling after a signed rotation and a later unsigned repin
// receives the connected key chain that ends at the active pin. Transitions
// from before the repin stay retained for verification but are not offered as
// an enrollment path, and a later signed rotation extends the new chain.
func TestRelayEnrollmentAfterUnsignedRepin(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		fixture := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(ctx, fixture.private.ID, "admin")
		require.NoError(t, err)
		pins := make([]db.RootKeyPin, 4)
		keys := make([]ed25519.PrivateKey, 4)
		for i := range pins {
			public, private, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			pins[i] = db.RootKeyPin{ProjectUID: fixture.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			keys[i] = private
		}
		require.NoError(t, store.PinRootAuthority(ctx, pins[0]))
		rotation, err := db.SignRootKeyTransition(pins[0], pins[1], keys[0])
		require.NoError(t, err)
		require.NoError(t, store.RotateRootAuthority(ctx, rotation))
		require.NoError(t, store.(db.RootRepinStorage).RepinRootAuthority(ctx, pins[1].KeyID, pins[2], true))

		enroll := func(key ed25519.PrivateKey, spoke, token string) api.RelayHandshake {
			t.Helper()
			server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, RootSigning: daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key})})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			endpoint := httptest.NewServer(server.Handler())
			t.Cleanup(endpoint.Close)
			body := map[string]any{"project_id": fixture.private.ID, "spoke_instance_uid": spoke, "token": token, "capabilities": "claim,pull,push", "relay": map[string]any{"protocol_version": 1, "serve_downstream": true}}
			code, _, raw := projectAccessFixture{store: store, server: endpoint}.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
			require.Equal(t, http.StatusOK, code, string(raw))
			var grant api.FederationEnrollmentOut
			require.NoError(t, json.Unmarshal(raw, &grant))
			require.NotNil(t, grant.Relay)
			downstream, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "downstream.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, downstream.Close()) })
			require.NoError(t, federationcoord.ValidateRelayRootAuthority(ctx, downstream, *grant.Relay), "a fresh relay must accept the advertised root")
			return *grant.Relay
		}
		afterRepin := enroll(keys[2], "00000000000000000000000002", "narrow-enrollment-test-token")
		require.Equal(t, pins[2].KeyID, afterRepin.Root.KeyID)
		existing, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "existing.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, existing.Close()) })
		_, err = existing.CreateProjectWithUID(ctx, "relay-copy", fixture.private.UID)
		require.NoError(t, err)
		require.NoError(t, existing.PinRootAuthority(ctx, pins[1]))
		require.Error(t, federationcoord.ValidateRelayRootAuthority(ctx, existing, afterRepin), "a relay that pinned the previous key cannot re-enroll after an unsigned repin")

		next, err := db.SignRootKeyTransition(pins[2], pins[3], keys[2])
		require.NoError(t, err)
		require.NoError(t, store.RotateRootAuthority(ctx, next))
		afterRotation := enroll(keys[3], "00000000000000000000000003", "second-enrollment-test-token")
		require.Equal(t, pins[2].KeyID, afterRotation.EnrollmentRoot.KeyID)
		require.Equal(t, []db.RootKeyTransition{next}, afterRotation.RootKeyTransitions)
		history, err := store.RootKeyTransitions(ctx, fixture.private.UID)
		require.NoError(t, err)
		require.Len(t, history, 2, "pre-repin transitions stay retained for verification")
	})
}

// A relay asking a hub that lost its root key for a reset checkpoint learns
// why the hub cannot sign and what its operator must run, instead of a
// generic storage failure.
func TestRelayResetReportsUnavailableRootKey(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		fixture := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(ctx, fixture.private.ID, "admin")
		require.NoError(t, err)
		_, private, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		start := func(signer db.RootAttributionSigner) projectAccessFixture {
			server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, RootSigning: daemon.NewRootSigning(signer)})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			endpoint := httptest.NewServer(server.Handler())
			t.Cleanup(endpoint.Close)
			return projectAccessFixture{store: store, server: endpoint}
		}
		withKey := start(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private})
		body := map[string]any{"project_id": fixture.private.ID, "spoke_instance_uid": "00000000000000000000000002", "token": "narrow-enrollment-test-token", "capabilities": "claim,pull,push", "relay": map[string]any{"protocol_version": 1, "serve_downstream": true}}
		code, _, raw := withKey.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
		require.Equal(t, http.StatusOK, code, string(raw))

		withoutKey := start(db.RootAttributionSigner{AuthorityUID: store.InstanceUID()})
		code, _, raw = withoutKey.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation/relay/reset", fixture.private.ID), "", nil, map[string]string{"Authorization": "Bearer narrow-enrollment-test-token"})
		require.Equal(t, http.StatusConflict, code, string(raw))
		require.Contains(t, string(raw), "root_signing_key_unavailable")
		require.Contains(t, string(raw), "kata federation root-key repin --project restricted-project")
	})
}

// An unsigned repin strands every relay that pinned the old key. It is refused
// while the project has active relay enrollments unless the operator accepts
// that explicitly, and the refusal changes nothing.
func TestRootKeyRepinRefusesToStrandRelays(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newRootRepinFixture(t, store, nil)
		f.enrollRelay(t)

		code, preview, raw := f.repin(t, map[string]any{"preflight": true})
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "unsigned_repin", preview.Method)
		require.Equal(t, int64(1), preview.ActiveRelayEnrollments)
		require.Contains(t, preview.Consequence, "--strand-relays")

		code, _, raw = f.repin(t, map[string]any{})
		require.Equal(t, http.StatusConflict, code, raw)
		require.Contains(t, raw, "root_repin_strands_relays")
		require.Contains(t, raw, "--strand-relays")
		pin, err := store.RootAuthority(t.Context(), f.hub.UID)
		require.NoError(t, err)
		require.Equal(t, f.oldPin, pin, "a refused repin leaves the pin unchanged")
		_, err = os.Stat(f.keyFile)
		require.ErrorIs(t, err, os.ErrNotExist, "a refused repin creates no key")
		status, _ := f.createIssue(t, f.hub)
		require.Equal(t, http.StatusConflict, status, "a refused repin leaves the project unavailable")

		code, result, raw := f.repin(t, map[string]any{"strand_relays": true})
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "repinned", result.Status)
		require.Equal(t, int64(1), result.ActiveRelayEnrollments)
		pin, err = store.RootAuthority(t.Context(), f.hub.UID)
		require.NoError(t, err)
		require.Equal(t, result.KeyID, pin.KeyID)
		count, err := store.CountActiveFederationEnrollments(t.Context(), f.hub.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), count, "repin reports the stranded grants but never revokes them")
		status, _ = f.createIssue(t, f.hub)
		require.Equal(t, http.StatusOK, status)
	})
}
