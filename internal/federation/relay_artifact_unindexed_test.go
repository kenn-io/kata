package federation_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
)

// R7: PostgreSQL's halfvec cap must not discard valid portable artifacts or
// cause document embedding calls. A PostgreSQL relay forwards them both ways.
func TestArtifactStoredUnindexedPostgres(t *testing.T) {
	ctx := t.Context()
	root := newRelayMatrixNode(t, "sqlite", "root-member", true)
	relay := newRelayMatrixNode(t, "postgres", "relay-member", true)
	leaf := newRelayMatrixNode(t, "sqlite", "leaf-member", true)
	project, err := root.store.CreateProject(ctx, "unindexed-artifact-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	enrollRelayMatrixReplica(t, root, relay, "unindexed-relay", true)
	enrollRelayMatrixReplica(t, relay, leaf, "unindexed-leaf", false)
	issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Portable large vector", Author: "assistant"})
	require.NoError(t, err)
	syncRelayMatrixNode(t, relay)
	syncRelayMatrixNode(t, leaf)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(provider.Close)
	client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 4001})
	require.NoError(t, err)
	var artifacts []embedding.EmbeddingArtifact
	for _, origin := range []*relayMatrixNode{root, leaf} {
		identity, err := client.ArtifactIdentity(project.UID, issue.UID, origin.store.InstanceUID())
		require.NoError(t, err)
		values := make([]float32, 4001)
		values[0] = 1
		artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{values})
		require.NoError(t, err)
		durable, err := origin.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
		require.NoError(t, err)
		require.True(t, durable)
		artifacts = append(artifacts, artifact)
	}
	for range 3 {
		syncRelayMatrixNode(t, relay)
		syncRelayMatrixNode(t, leaf)
	}
	for _, node := range []*relayMatrixNode{root, relay, leaf} {
		for _, artifact := range artifacts {
			got, err := node.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
			require.NoError(t, err)
			require.Equal(t, artifact, got)
		}
	}
	ix, err := vector.OpenPostgres(ctx, relay.store.(*pgstore.Store).DB)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	worker := daemon.NewReconciler(relay.store, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
	require.Eventually(t, func() bool { return worker.Health().LastError != "" }, 10*time.Second, 10*time.Millisecond)
	stop()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Contains(t, worker.Health().LastError, "4,000")
	require.Zero(t, calls.Load(), "unsupported local index dimensions never call the provider")
	server := daemon.NewServer(daemon.ServerConfig{DB: relay.store, VectorIndex: ix, Embedder: client, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/federation/status", relay.project.ID), fmt.Sprintf("/api/v1/projects/%d/search?q=Portable&mode=lexical", relay.project.ID)} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+path, nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+relay.userToken)
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode, string(body))
		if path == fmt.Sprintf("/api/v1/projects/%d/federation/status", relay.project.ID) {
			require.Contains(t, string(body), `"state":"stored_unindexed"`)
			require.NotContains(t, string(body), "vector_bytes")
		} else {
			require.Contains(t, string(body), issue.UID)
		}
	}
	require.Zero(t, calls.Load(), "status and lexical fallback never embed")
}
