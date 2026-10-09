package federation_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/testenv"
)

type relayMatrixNode struct {
	store              db.Storage
	http               *httptest.Server
	signer             db.RootAttributionSigner
	userToken, account string
	project            db.Project
	credential         config.FederationCredential
	credentials        *fakeCredentialStore
}

func newRelayMatrixNode(t *testing.T, backend, account string, withVectors ...bool) *relayMatrixNode {
	t.Helper()
	var store db.Storage
	if backend == "sqlite" {
		s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
		require.NoError(t, err)
		store = s
	} else {
		var dsn string
		var cleanup func()
		if len(withVectors) > 0 && withVectors[0] {
			dsn, cleanup = testenv.NewPostgresWithPgvectorContainer(t, t.Context())
		} else {
			dsn, cleanup = testenv.NewPostgresContainer(t, t.Context())
		}
		t.Cleanup(cleanup)
		s, err := pgstore.Open(t.Context(), dsn)
		require.NoError(t, err)
		store = s
	}
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: account, AdminActor: "admin", PlaintextToken: "relay-matrix-user-test-token"})
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	node := &relayMatrixNode{store: store, signer: signer, userToken: "relay-matrix-user-test-token", account: account, credentials: credentials}
	server := daemon.NewServer(daemon.ServerConfig{DB: store, FederationCredentials: credentials, RootAttributionSigner: &node.signer, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	node.http = httpServer
	return node
}

func enrollRelayMatrixReplica(t *testing.T, upstream, local *relayMatrixNode, alias string, downstream bool) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"project_id": upstream.project.ID, "spoke_instance_uid": local.store.InstanceUID(), "capabilities": "claim,pull,push", "relay": api.RelayEnrollmentOptions{ProtocolVersion: 1, ServeDownstream: downstream}})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, upstream.http.URL+"/api/v1/federation/enrollments", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+upstream.userToken)
	response, err := upstream.http.Client().Do(request)
	require.NoError(t, err)
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	var grant api.FederationEnrollmentOut
	require.NoError(t, json.Unmarshal(raw, &grant))
	require.NotNil(t, grant.Relay)
	project, err := local.store.CreateProject(t.Context(), alias)
	require.NoError(t, err)
	adopted, err := local.store.AdoptProjectIntoFederation(t.Context(), db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: upstream.http.URL, HubProjectID: upstream.project.ID, HubProjectUID: upstream.project.UID, Actor: grant.Actor, EmptyOnly: true})
	require.NoError(t, err)
	local.project = adopted.Project
	binding := adopted.Binding
	binding.Enabled = true
	binding.PushEnabled = true
	_, err = local.store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	require.NoError(t, federation.PinRelayRootAuthority(t.Context(), local.store, *grant.Relay))
	_, err = local.store.SetRelayBindingConfig(t.Context(), local.project.ID, db.RelayBindingConfig{ProtocolVersion: grant.Relay.ProtocolVersion, BindingUID: grant.Relay.BindingUID, UpstreamInstanceUID: grant.Relay.UpstreamInstanceUID, AuthorityUID: grant.Relay.Root.AuthorityUID, HubPath: grant.Relay.HubPath, LocalActor: local.account, ServeDownstream: downstream, ResetEpoch: grant.Relay.ResetEpoch})
	require.NoError(t, err)
	local.credential = config.FederationCredential{HubURL: upstream.http.URL, HubProjectID: upstream.project.ID, Token: grant.Token, Actor: grant.Actor, Capabilities: grant.Capabilities}
	require.NoError(t, local.credentials.StoreFederationCredential(t.Context(), local.project.UID, local.credential))
}

func syncRelayMatrixNode(t *testing.T, node *relayMatrixNode) {
	t.Helper()
	binding, err := node.store.FederationBindingByProject(t.Context(), node.project.ID)
	require.NoError(t, err)
	require.NoError(t, federation.SyncFederationOnce(t.Context(), node.store, binding, node.credential))
}

var errRelayPullPostCommitLookup = errors.New("post-commit event lookup failed")

type relayPullEventLookupFailureStorage struct {
	db.Storage
	db.RelayArtifactStorage
	db.RelayResetStore
}

func (s relayPullEventLookupFailureStorage) EventsByUIDs(context.Context, int64, []string) ([]db.Event, error) {
	return nil, errRelayPullPostCommitLookup
}

func TestRelayPullPublishesAcceptedEventsWithoutPostCommitLookup(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "company-member")
			personal := newRelayMatrixNode(t, backend, "personal-member")
			project, err := root.store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			syncRelayMatrixNode(t, personal)

			issue, source, err := root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "source-assistant", Title: "upstream event"})
			require.NoError(t, err)
			binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			var callbackProjectID int64
			var published []db.Event
			artifactStore, ok := personal.store.(db.RelayArtifactStorage)
			require.True(t, ok)
			resetStore, ok := personal.store.(db.RelayResetStore)
			require.True(t, ok)
			syncErr := federation.SyncFederationOnceWithPulledEvents(t.Context(), relayPullEventLookupFailureStorage{Storage: personal.store, RelayArtifactStorage: artifactStore, RelayResetStore: resetStore}, binding, personal.credential, clientpkg.Opts{}, func(projectID int64, events []db.Event) {
				callbackProjectID = projectID
				published = append(published, events...)
			})

			mirrored, err := personal.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err, "the received event must already be committed")
			require.Equal(t, issue.UID, mirrored.UID)
			require.NoError(t, syncErr)
			require.Equal(t, personal.project.ID, callbackProjectID)
			require.Len(t, published, 1)
			require.Equal(t, source.UID, published[0].UID)
		})
	}
}

// R1/R4/R5/A2: real HTTP through two hubs, two leaf devices, and a second
// personal relay. Each of the three backend roles independently varies.
// Artifact reuse, reset/crash/privacy and producer gates extend this topology;
// passing event/receipt transport alone does not complete those gates.
func TestRelayBidirectionalBackendMatrix(t *testing.T) {
	for _, rootBackend := range []string{"sqlite", "postgres"} {
		for _, relayBackend := range []string{"sqlite", "postgres"} {
			for _, leafBackend := range []string{"sqlite", "postgres"} {
				t.Run(fmt.Sprintf("%s_%s_%s", rootBackend, relayBackend, leafBackend), func(t *testing.T) {
					root := newRelayMatrixNode(t, rootBackend, "company-member")
					personal := newRelayMatrixNode(t, relayBackend, "personal-member")
					second := newRelayMatrixNode(t, relayBackend, "second-member")
					firstLeaf := newRelayMatrixNode(t, leafBackend, "first-member")
					secondLeaf := newRelayMatrixNode(t, leafBackend, "second-member")
					project, err := root.store.CreateProject(t.Context(), "shared-project")
					require.NoError(t, err)
					root.project = project
					_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
					require.NoError(t, err)
					public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
					pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
					require.NoError(t, root.store.PinRootAuthority(t.Context(), pin))
					enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
					enrollRelayMatrixReplica(t, root, second, "second-alias", true)
					enrollRelayMatrixReplica(t, personal, firstLeaf, "first-alias", false)
					enrollRelayMatrixReplica(t, personal, secondLeaf, "second-leaf-alias", false)
					issue, source, err := firstLeaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: firstLeaf.project.ID, Author: "source-assistant", Title: "Leaf-origin task"})
					require.NoError(t, err)
					syncRelayMatrixNode(t, firstLeaf)
					syncRelayMatrixNode(t, personal)
					for _, node := range []*relayMatrixNode{personal, firstLeaf, secondLeaf, second} {
						syncRelayMatrixNode(t, node)
					}
					for _, node := range []*relayMatrixNode{root, personal, firstLeaf, secondLeaf, second} {
						mirrored, err := node.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
						require.NoError(t, err)
						require.Equal(t, "source-assistant", mirrored.Author)
						require.Equal(t, "company-member", mirrored.AccountableActor)
						proof, err := node.store.EntityAttribution(t.Context(), project.UID, "issue", issue.UID)
						require.NoError(t, err)
						require.NoError(t, db.VerifyRootReceipt(pin, proof))
						require.Equal(t, source.UID, proof.EventUID)
						require.Equal(t, source.ContentHash, proof.ContentHash)
					}
					_, _, err = root.store.CreateComment(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateCommentParams{IssueID: issueIDForRelayMatrix(t, root, issue.UID), Author: root.account, Body: "Root-origin reply", Teammate: "reviewer"})
					require.NoError(t, err)
					for _, node := range []*relayMatrixNode{personal, firstLeaf, secondLeaf, second} {
						syncRelayMatrixNode(t, node)
						syncRelayMatrixNode(t, node)
					}
					for _, node := range []*relayMatrixNode{root, personal, firstLeaf, secondLeaf, second} {
						comments, err := node.store.CommentsByIssue(t.Context(), issueIDForRelayMatrix(t, node, issue.UID))
						require.NoError(t, err)
						require.Len(t, comments, 1)
						require.Equal(t, "Root-origin reply", comments[0].Body)
						require.Equal(t, "company-member", comments[0].AccountableActor)
						require.Equal(t, "reviewer", comments[0].Teammate)
					}
				})
			}
		}
	}
}

func issueIDForRelayMatrix(t *testing.T, node *relayMatrixNode, uid string) int64 {
	t.Helper()
	issue, err := node.store.IssueByUID(t.Context(), uid, db.IncludeDeletedYes)
	require.NoError(t, err)
	return issue.ID
}
