package vector_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestPostgresPgvectorIndexesAndRanksSemanticIssue(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "coordinate mutations", Body: "serialize concurrent writers", Author: "operator",
	})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "refresh dashboard", Body: "update colors and typography", Author: "operator",
	})
	require.NoError(t, err)

	idx, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	embedder := mappedEmbedder(t, func(input string) []float32 {
		if input == "database contention" || strings.Contains(input, "serialize concurrent writers") {
			return []float32{1, 0}
		}
		return []float32{0, 1}
	})
	reconcileCtx, cancel := context.WithCancel(ctx)
	reconciler := daemon.NewReconciler(store, idx, embedder, daemon.ReconcilerConfig{
		BatchSize: 64, SweepEvery: time.Hour,
	})
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(reconcileCtx) }()
	require.Eventually(t, func() bool {
		health := reconciler.Health()
		return health.LastSuccessAt != nil && health.Backlog == 0 && health.Embedded == 2
	}, 5*time.Second, 10*time.Millisecond, "PostgreSQL reconciler did not finish pgvector backfill")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "PostgreSQL reconciler did not stop")
	}
	key := embedder.Generation().Fingerprint()

	queryVectors, err := embedder.EncodeFunc()(ctx, []string{"database contention"})
	require.NoError(t, err)
	hits, err := idx.Query(ctx, key, queryVectors[0], 2)
	require.NoError(t, err)
	rolledUp := kitvec.RollupByDocument(hits)
	require.NotEmpty(t, rolledUp)
	assert.Equal(t, target.UID, rolledUp[0].Doc)
	assert.Greater(t, rolledUp[0].Score, float32(0.99))
}

func mappedEmbedder(t *testing.T, vectorFor func(string) []float32) *embedding.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		data := make([]map[string]any, len(request.Input))
		for i, input := range request.Input {
			data[i] = map[string]any{"embedding": vectorFor(input)}
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(server.Close)
	client, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "pgvector-e2e", Dims: 2})
	require.NoError(t, err)
	return client
}
