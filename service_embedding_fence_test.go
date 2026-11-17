package kata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestServiceEmbeddingHonorsRevokedWorkerFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	revoked := atomic.Bool{}
	requests := make(chan []string, 4)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		requests <- input.Input
		data := make([]map[string]any, len(input.Input))
		for i := range input.Input {
			data[i] = map[string]any{"embedding": []float32{1, 0}, "index": i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(endpoint.Close)
	denied := errors.New("worker authority revoked")
	service, err := New(ctx, Config{
		DSN:      dsn,
		Postgres: PostgresConfig{Schema: "kata", SchemaMode: PostgresSchemaBootstrap},
		Access:   allowWorkerFenceAccess{},
		WorkerTransactionFence: func(context.Context, Transaction) error {
			if revoked.Load() {
				return denied
			}
			return nil
		},
		Embeddings: EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test", Dims: 2, BatchSize: 1, TrustPrivateNetwork: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project, err := service.store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, _, err = service.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "first issue", Author: "user-a"})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	var firstRequest []string
	select {
	case firstRequest = <-requests:
	case runErr := <-done:
		require.NoError(t, runErr)
	case <-ctx.Done():
		require.FailNow(t, "service did not send the initial issue to the provider")
	}
	require.Len(t, firstRequest, 1)
	require.Contains(t, firstRequest[0], "first issue")
	//nolint:kennlint // sleeptest: Postgres and the worker require real I/O.
	require.Eventually(t, func() bool { return service.embeddingReconciler.Health().Embedded == 1 }, 5*time.Second, 10*time.Millisecond)

	revoked.Store(true)
	_, _, err = service.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "revoked issue", Author: "user-a"})
	require.NoError(t, err)
	service.embeddingReconciler.Wake()

	select {
	case input := <-requests:
		cancel()
		runErr := <-done
		require.Failf(t, "revoked issue reached the embedding provider", "input=%q, Run error=%v", strings.Join(input, " | "), runErr)
	case runErr := <-done:
		require.ErrorIs(t, runErr, denied)
		select {
		case input := <-requests:
			require.Failf(t, "revoked issue reached the embedding provider", "input=%q", strings.Join(input, " | "))
		default:
		}
	case <-time.After(5 * time.Second):
		cancel()
		runErr := <-done
		require.Fail(t, "worker did not stop after authority was revoked", runErr)
	}
	var mirrored, chunks int
	require.NoError(t, service.store.(*pgstore.Store).DB.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM issue_vector_mirror WHERE content LIKE '%revoked issue%'), (SELECT count(*) FROM issue_vector_chunks c JOIN issue_vector_mirror m ON m.issue_uid = c.issue_uid WHERE m.content LIKE '%revoked issue%')`).Scan(&mirrored, &chunks))
	require.Zero(t, mirrored)
	require.Zero(t, chunks)
}
