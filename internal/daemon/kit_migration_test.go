package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestKitMigrationReusesPersistedEmbeddingsForIndexingAndServing(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "repair login", Body: "session expired", Author: "example-author",
	})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "vectors.db")
	index, err := vector.Open(ctx, path)
	require.NoError(t, err)
	_, err = index.RefreshMirror(ctx, store)
	require.NoError(t, err)
	legacy := kitvec.Generation{Model: "example-model", Dimensions: 2, Params: map[string]string{"recipe": "2", "salt": "example-weights"}}
	key := legacy.Fingerprint()
	require.NoError(t, index.EnsureBuilding(ctx, key, legacy))
	_, err = index.Fill(ctx, key, func(_ context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for position := range texts {
			vectors[position] = []float32{1, 0}
		}
		return vectors, nil
	}, 0, nil, nil)
	require.NoError(t, err)
	require.NoError(t, index.CutOver(ctx, key))
	require.NoError(t, index.Close())
	index, err = vector.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	var requests [][]string
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		requests = append(requests, body.Input)
		require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{1, 0}}}}))
	}))
	t.Cleanup(provider.Close)
	client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2, Salt: "example-weights"})
	require.NoError(t, err)
	matches, err := client.Space().Matches(key)
	require.NoError(t, err)
	require.True(t, matches)
	require.NoError(t, NewReconciler(store, index, client, ReconcilerConfig{}).reconcileOnce(ctx))
	require.Empty(t, requests, "reconciliation must not encode an already covered legacy generation")
	active, ok, err := index.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, key, active)
	result, err := hybridSearch(ctx, store, index, client, hybridParams{
		ProjectID: project.ID, Query: "login", Limit: 10, Requested: "hybrid",
	})
	require.NoError(t, err)
	require.Equal(t, modeHybrid, result.Mode)
	require.False(t, result.Degraded)
	require.Len(t, result.Hits, 1)
	require.Equal(t, issue.UID, result.Hits[0].Issue.UID)
	require.Contains(t, result.Hits[0].MatchedIn, "semantic")
	require.Equal(t, [][]string{{"login\n\n"}}, requests, "serving must use stored document vectors and encode only the query")

	updatedTitle := "repair session"
	updated, _, changed, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &updatedTitle, Actor: "example-author"})
	require.NoError(t, err)
	require.True(t, changed)
	_, err = index.RefreshMirror(ctx, store)
	require.NoError(t, err)
	hits, err := index.Query(ctx, key, kitvec.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Empty(t, hits, "stale document vectors must not score the edited revision")
	require.NoError(t, NewReconciler(store, index, client, ReconcilerConfig{}).reconcileOnce(ctx))
	require.Equal(t, [][]string{{"login\n\n"}, {"repair session\n\nsession expired"}}, requests)
	result, err = hybridSearch(ctx, store, index, client, hybridParams{
		ProjectID: project.ID, Query: "session", Limit: 10, Requested: "hybrid",
	})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	require.Equal(t, updated.Revision, result.Hits[0].Issue.Revision)
	_, _, _, err = store.SoftDeleteIssue(ctx, issue.ID, "example-author")
	require.NoError(t, err)
	result, err = hybridSearch(ctx, store, index, client, hybridParams{
		ProjectID: project.ID, Query: "session", Limit: 10, Requested: "hybrid",
	})
	require.NoError(t, err)
	require.Empty(t, result.Hits, "deleted canonical issues must not leak before mirror reconciliation")
	before := len(requests)
	require.NoError(t, NewReconciler(store, index, client, ReconcilerConfig{}).reconcileOnce(ctx))
	require.Len(t, requests, before, "deletion must not send document text to the provider")
	hits, err = index.Query(ctx, key, kitvec.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Empty(t, hits)
}
