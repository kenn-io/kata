package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

func TestEmbeddingProjectPolicyRealClientDispatch(t *testing.T) {
	ctx := t.Context()
	store := newReconcilerTestStore(t)
	idx := openTestVectorIndex(t)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "example task", Author: "tester"})
	require.NoError(t, err)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	defer server.Close()
	client, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "m", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
	require.NoError(t, err)
	calls := 0
	r := NewReconciler(store, idx, client, ReconcilerConfig{EmbeddingProjectAllowed: func(_ context.Context, uid string) (bool, error) {
		require.Equal(t, project.UID, uid)
		calls++
		return calls <= 2, nil
	}})
	require.NoError(t, r.reconcileOnce(ctx))
	require.GreaterOrEqual(t, calls, 3)
	require.Zero(t, requests.Load())
}

func TestEmbeddingProjectPolicyQuery(t *testing.T) {
	ctx := t.Context()
	store := newReconcilerTestStore(t)
	idx := openTestVectorIndex(t)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "example task", Author: "tester"})
	require.NoError(t, err)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "m", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
	require.NoError(t, err)
	for _, policyErr := range []error{nil, errors.New("policy unavailable")} {
		for _, mode := range []string{"semantic", "hybrid", "auto"} {
			result, err := hybridSearch(ctx, store, idx, client, hybridParams{ProjectID: project.ID, Query: "example", Limit: 10, Requested: mode, EmbeddingProjectAllowed: func(_ context.Context, uid string) (bool, error) {
				require.Equal(t, project.UID, uid)
				return false, policyErr
			}})
			if mode == "auto" {
				require.NoError(t, err)
				require.True(t, result.Degraded)
				require.Len(t, result.Hits, 1)
			} else {
				var modeErr *modeError
				require.ErrorAs(t, err, &modeErr)
				require.Equal(t, 503, modeErr.Status())
			}
		}
	}
	require.Zero(t, requests.Load())
}

func TestEmbeddingProjectPolicyDispatchError(t *testing.T) {
	expected := errors.New("policy unavailable")
	r := NewReconciler(nil, nil, nil, ReconcilerConfig{EmbeddingProjectAllowed: func(context.Context, string) (bool, error) { return false, expected }})
	allowed, err := r.projectGenerationAllowed(t.Context(), "project", embedding.ArtifactIdentity{}, false)
	require.False(t, allowed)
	require.ErrorIs(t, err, expected)
}
