package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// Changing literal affixes must not score new queries against old vectors:
// search falls back to lexical until the new generation fills, then queries
// carry the configured query affixes.
func TestLiteralEmbeddingGenerationCutover(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "repair login", Body: "session expired", Author: "example-author",
	})
	require.NoError(t, err)
	var inputs [][]string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		inputs = append(inputs, req.Input)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"embedding": []float32{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer provider.Close()
	old, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2})
	require.NoError(t, err)
	idx := openTestVectorIndex(t)
	fillGeneration(ctx, t, store, idx, old)
	current, err := embedding.New(embedding.Config{
		BaseURL: provider.URL, Model: "example-model", Dims: 2,
		DocumentPrefix: "title: none | text: ", DocumentSuffix: " document end ",
		QueryPrefix: "task: search result | query: ", QuerySuffix: " query end ",
	})
	require.NoError(t, err)
	inputs = nil

	result, err := hybridSearch(ctx, store, idx, current, hybridParams{ProjectID: project.ID, Query: "repair", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, modeLexical, result.Mode)
	require.True(t, result.Degraded)
	_, err = hybridSearch(ctx, store, idx, current, hybridParams{
		ProjectID: project.ID, Query: "repair", Limit: 10, Requested: "semantic",
	})
	var modeErr *modeError
	require.ErrorAs(t, err, &modeErr)
	require.Equal(t, http.StatusServiceUnavailable, modeErr.Status())
	require.ErrorContains(t, err, "embedding configuration changed; new index is backfilling")
	require.Empty(t, inputs, "no query may be embedded against the old generation")

	fillGeneration(ctx, t, store, idx, current)
	require.Equal(t, [][]string{{"title: none | text: repair login\n\nsession expired document end "}}, inputs)
	inputs = nil
	result, err = hybridSearch(ctx, store, idx, current, hybridParams{
		ProjectID: project.ID, Query: "repair", Limit: 10, Requested: "semantic",
	})
	require.NoError(t, err)
	require.Equal(t, modeSemantic, result.Mode)
	require.Len(t, result.Hits, 1)
	require.Equal(t, target.UID, result.Hits[0].Issue.UID)
	require.Equal(t, [][]string{{"task: search result | query: repair query end "}}, inputs)
}
