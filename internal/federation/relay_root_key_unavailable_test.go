package federation_test

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A relay pushing to a hub that restarted without its root key gets the
// recovery error the hub operator needs, and its write stays pending.
func TestRelayAcceptanceReportsUnavailableRootKey(t *testing.T) {
	root := newRelayMatrixNode(t, "sqlite", "company-member")
	personal := newRelayMatrixNode(t, "sqlite", "personal-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	enrollRelayMatrixReplica(t, root, personal, "personal-alias", false)
	syncRelayMatrixNode(t, personal)

	root.useRootKey(t, nil)
	_, _, err = personal.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: personal.project.ID, Author: "leaf-agent", Title: "Waits for the hub's key"})
	require.NoError(t, err)
	binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
	require.NoError(t, err)
	// Sync stops earlier, at the metadata handshake. Post the retained batch
	// directly, as a relay already past its handshake would.
	pending, err := personal.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.NotEmpty(t, pending)
	before, err := root.store.MaxEventID(t.Context())
	require.NoError(t, err)
	code, raw := postRelayCredentialRequest(t, root.http, fmt.Sprintf("/api/v1/projects/%d/federation/relay:accept", root.project.ID), personal.credential.Token, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: pending})
	require.Equal(t, http.StatusConflict, code, string(raw))
	require.Contains(t, string(raw), "root_signing_key_unavailable")
	require.Contains(t, string(raw), "kata federation root-key repin --project shared-project")
	after, err := root.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "the refused write never commits on the hub")
}
