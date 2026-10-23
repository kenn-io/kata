package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

func TestRecaptureHubArtifactsFromCoveredVectors(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	idx := openTestVectorIndex(t)
	emb := newReconcilerArtifactEmbedder(t, "example-model")
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Already indexed", Body: "before artifact retention", Author: "member",
	})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID,
		HubProjectUID: project.UID, Actor: "member", Enabled: true,
	})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	recipe, err := emb.ArtifactIdentity("", "", store.InstanceUID())
	require.NoError(t, err)
	key := emb.Generation().Fingerprint()
	require.NoError(t, idx.EnsureBuilding(ctx, key, emb.Generation()))
	_, err = idx.Fill(ctx, key, func(context.Context, []string) ([][]float32, error) {
		return [][]float32{{1, 0}}, nil
	}, 0, nil, nil)
	require.NoError(t, err)
	artifacts := db.EmbeddingArtifactStorage(store)
	manifests, err := artifacts.EmbeddingArtifactManifests(ctx, project.UID, 10)
	require.NoError(t, err)
	require.Empty(t, manifests)

	r := NewReconciler(store, idx, emb, ReconcilerConfig{})
	require.NoError(t, r.selectIdleHubProducers(ctx, recipe))
	require.NoError(t, r.recaptureHubArtifacts(ctx, key, recipe))
	manifests, err = artifacts.EmbeddingArtifactManifests(ctx, project.UID, 10)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	artifact, err := artifacts.StoredEmbeddingArtifact(ctx, project.UID, manifests[0].Digest)
	require.NoError(t, err)
	require.NoError(t, embedding.ValidateArtifact(artifact, embedding.EmbedText(issue.Title, issue.Body), recipe.RecipeFingerprint))
}
