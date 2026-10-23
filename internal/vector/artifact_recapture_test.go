package vector_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
)

func TestRecaptureArtifactsFromCoveredVectors(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx, err := vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, idx.Close()) })

	project, err := store.CreateProject(ctx, "recapture-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Already indexed", Body: "before artifact retention", Author: "member",
	})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	client, err := embedding.New(embedding.Config{BaseURL: "https://encoder.example/v1", Model: "example-model", Dims: 2})
	require.NoError(t, err)
	recipe, err := client.ArtifactIdentity("", "", store.InstanceUID())
	require.NoError(t, err)
	key := client.Generation().Fingerprint()
	require.NoError(t, idx.EnsureBuilding(ctx, key, client.Generation()))
	_, err = idx.Fill(ctx, key, func(context.Context, []string) ([][]float32, error) {
		return [][]float32{{0.6, 0.8}}, nil
	}, 0, nil, nil)
	require.NoError(t, err)
	manifests, err := store.EmbeddingArtifactManifests(ctx, project.UID, 10)
	require.NoError(t, err)
	require.Empty(t, manifests, "pre-retention vectors should begin without artifacts")

	recaptured, err := idx.RecaptureArtifacts(ctx, key, store, recipe, project.UID, []string{issue.UID})
	require.NoError(t, err)
	require.Equal(t, 1, recaptured)
	manifests, err = store.EmbeddingArtifactManifests(ctx, project.UID, 10)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	artifact, err := store.StoredEmbeddingArtifact(ctx, project.UID, manifests[0].Digest)
	require.NoError(t, err)
	require.NoError(t, embedding.ValidateArtifact(artifact, embedding.EmbedText(issue.Title, issue.Body), recipe.RecipeFingerprint))

	recaptured, err = idx.RecaptureArtifacts(ctx, key, store, recipe, project.UID, []string{issue.UID})
	require.NoError(t, err)
	require.Zero(t, recaptured, "a matching artifact should not be retained repeatedly")
}
