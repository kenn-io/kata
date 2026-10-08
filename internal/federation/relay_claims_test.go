package federation_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

func TestRelayClaimHolderMappingSupportsLeafMutationAndPendingRetry(t *testing.T) {
	t.Run("online acquire then edit", func(t *testing.T) {
		_, _, leaf, issue := newRelayClaimChain(t)
		status, acquired := relayClaimAction(t, leaf, issue.UID, "acquire", "cli")
		require.Equal(t, http.StatusOK, status)
		require.True(t, acquired.Granted)
		require.NotNil(t, acquired.Lease)
		leafBinding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
		require.NoError(t, err)
		require.Equal(t, leaf.store.InstanceUID(), acquired.Lease.HolderInstanceUID)
		require.Equal(t, leafBinding.Actor, acquired.Lease.Holder)

		body, err := json.Marshal(map[string]string{"actor": leaf.account, "title": "edited through relay claim"})
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPatch,
			fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", leaf.http.URL, leaf.project.ID, issue.ShortID), bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+leaf.userToken)
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode, string(responseBody))
		updated, err := leaf.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, "edited through relay claim", updated.Title)
	})

	t.Run("offline acquire resolves through relay", func(t *testing.T) {
		_, relay, leaf, issue := newRelayClaimChain(t)
		binding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
		require.NoError(t, err)
		const offlineURL = "http://127.0.0.1:1"
		binding, err = leaf.store.RebindFederationBinding(t.Context(), db.RebindFederationBindingParams{
			ProjectID: leaf.project.ID, ExpectedHubURL: binding.HubURL,
			HubProjectID: binding.HubProjectID, HubProjectUID: binding.HubProjectUID,
			TargetHubURL: offlineURL,
		})
		require.NoError(t, err)
		status, pendingResponse := relayClaimAction(t, leaf, issue.UID, "acquire", "cli")
		require.Equal(t, http.StatusOK, status)
		require.True(t, pendingResponse.Pending)
		require.NotEmpty(t, pendingResponse.RequestUID)

		binding, err = leaf.store.RebindFederationBinding(t.Context(), db.RebindFederationBindingParams{
			ProjectID: leaf.project.ID, ExpectedHubURL: offlineURL,
			HubProjectID: binding.HubProjectID, HubProjectUID: binding.HubProjectUID,
			TargetHubURL: relay.http.URL,
		})
		require.NoError(t, err)
		credential := leaf.credential
		credential.HubURL = relay.http.URL
		require.NoError(t, federation.RetryPendingClaimsOnce(
			context.Background(), leaf.store, binding, credential, clientpkg.Opts{},
		))
		pending, err := leaf.store.ListPendingClaimRequests(t.Context(), leaf.project.ID, 10)
		require.NoError(t, err)
		require.Empty(t, pending, "the relay must return the leaf holder tuple so the pending request resolves")
		claimStatus, err := leaf.store.ClaimStatus(t.Context(), leaf.project.ID, issue.UID, time.Now().UTC())
		require.NoError(t, err)
		require.True(t, claimStatus.Held)
		require.Equal(t, leaf.store.InstanceUID(), claimStatus.Holder.HolderInstanceUID)
		leafBinding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
		require.NoError(t, err)
		require.Equal(t, leafBinding.Actor, claimStatus.Holder.Holder)
	})
}

func newRelayClaimChain(t *testing.T) (*relayMatrixNode, *relayMatrixNode, *relayMatrixNode, db.Issue) {
	t.Helper()
	root := newRelayMatrixNode(t, "sqlite", "company-member")
	project, err := root.store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub,
		HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(),
		KeyID: db.RootPublicKeyID(public), PublicKey: public,
	}))
	issue, _, err := root.store.CreateIssue(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateIssueParams{
		ProjectID: project.ID, Author: root.account, Title: "claim target",
	})
	require.NoError(t, err)

	relay := newRelayMatrixNode(t, "sqlite", "personal-member")
	leaf := newRelayMatrixNode(t, "sqlite", "leaf-member")
	enrollRelayMatrixReplica(t, root, relay, "relay-project", true)
	syncRelayMatrixNode(t, relay)
	enrollRelayMatrixReplica(t, relay, leaf, "leaf-project", false)
	syncRelayMatrixNode(t, leaf)
	return root, relay, leaf, issue
}

// R4/A7: the root arbitrates; two leaves sharing an account and client label
// cannot renew or release each other's root lease through the personal relay.
func TestRelayRootClaimAuthority(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "company-member")
			personal := newRelayMatrixNode(t, backend, "personal-member")
			first := newRelayMatrixNode(t, backend, "leaf-member")
			second := newRelayMatrixNode(t, backend, "leaf-member")
			project, err := root.store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			enrollRelayMatrixReplica(t, personal, first, "first-alias", false)
			enrollRelayMatrixReplica(t, personal, second, "second-alias", false)
			owner := daemon.NewServer(daemon.ServerConfig{DB: personal.store, FederationCredentials: personal.credentials, Auth: config.AuthConfig{Token: "lifecycle-owner-test-token"}})
			t.Cleanup(func() { require.NoError(t, owner.Close()) })
			ownerHTTP := httptest.NewServer(owner.Handler())
			t.Cleanup(ownerHTTP.Close)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/federation/replicas/%d/actions/leave", ownerHTTP.URL, personal.project.ID), bytes.NewBufferString(`{"disposition":"archive","force":true,"actor":"local-owner"}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer lifecycle-owner-test-token")
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			_ = response.Body.Close()
			require.Equal(t, http.StatusConflict, response.StatusCode, "active descendants block archive before credential or projection changes")
			preserved, err := personal.store.ProjectByID(t.Context(), personal.project.ID)
			require.NoError(t, err)
			require.Nil(t, preserved.DeletedAt)
			issue, _, err := root.store.CreateIssue(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Author: root.account, Title: "Root lease isolation"})
			require.NoError(t, err)
			for _, node := range []*relayMatrixNode{personal, first, second} {
				syncRelayMatrixNode(t, node)
			}
			status, granted := relayClaimAction(t, first, issue.UID, "acquire", "cli")
			require.Equal(t, http.StatusOK, status)
			require.True(t, granted.Granted)
			require.NotNil(t, granted.Lease)
			require.Equal(t, personal.account, granted.Lease.Holder)
			rootState, err := root.store.ClaimStatusReadOnly(t.Context(), project.ID, issue.UID, time.Now().UTC())
			require.NoError(t, err)
			require.True(t, rootState.Held)
			require.Equal(t, granted.Lease.ClaimUID, rootState.Claim.ClaimUID)
			require.Equal(t, root.account, rootState.Claim.Holder)
			for _, action := range []string{"renew", "release"} {
				status, _ := relayClaimAction(t, second, issue.UID, action, "cli")
				require.Equal(t, http.StatusConflict, status, "second leaf cannot %s first leaf's root lease", action)
				status, _ = relayClaimAction(t, second, issue.UID, action, granted.Lease.ClientKind)
				require.Equal(t, http.StatusConflict, status, "copying the visible root client identity grants no leaf authority")
			}
			status, denied := relayClaimAction(t, second, issue.UID, "acquire", "cli")
			require.Equal(t, http.StatusOK, status)
			require.False(t, denied.Granted)
			status, renewed := relayClaimAction(t, first, issue.UID, "renew", "cli")
			require.Equal(t, http.StatusOK, status)
			require.True(t, renewed.Granted)
			require.Equal(t, granted.Lease.ClaimUID, renewed.Lease.ClaimUID)
			status, released := relayClaimAction(t, first, issue.UID, "release", "cli")
			require.Equal(t, http.StatusOK, status)
			require.True(t, released.Granted)
			status, next := relayClaimAction(t, second, issue.UID, "acquire", "cli")
			require.Equal(t, http.StatusOK, status)
			require.True(t, next.Granted)
			require.NotEqual(t, granted.Lease.ClientKind, next.Lease.ClientKind, "root holder tuple retains device isolation")
		})
	}
}

func relayClaimAction(t *testing.T, node *relayMatrixNode, issueUID, action, kind string) (int, api.ClaimActionResponseBody) {
	t.Helper()
	raw, err := json.Marshal(api.ClaimActionBody{Holder: "forged-holder-label", ClientKind: kind, ClaimKind: "timed", TTLSeconds: 300})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/lease/actions/%s", node.http.URL, node.project.ID, issueUID, action)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+node.userToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	require.NoError(t, err)
	var output api.ClaimActionResponseBody
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(data, &output))
	}
	return resp.StatusCode, output
}
