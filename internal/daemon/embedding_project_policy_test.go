package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	kitvec "go.kenn.io/kit/vector"
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

func TestEmbeddingProjectPolicyDispatchError(t *testing.T) {
	expected := errors.New("policy unavailable")
	r := NewReconciler(nil, nil, nil, ReconcilerConfig{EmbeddingProjectAllowed: func(context.Context, string) (bool, error) { return false, expected }})
	allowed, err := r.projectGenerationAllowed(t.Context(), "project", embedding.ArtifactIdentity{}, false)
	require.False(t, allowed)
	require.ErrorIs(t, err, expected)
}

func TestEmbeddingProjectPolicyBlocksHubQuery(t *testing.T) {
	ctx := t.Context()
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "shared")
	require.NoError(t, err)
	called := false
	_, err = hybridSearch(ctx, store, nil, nil, hybridParams{
		ProjectID: project.ID, Query: "example", Limit: 10, Requested: "semantic",
		EmbeddingProjectAllowed: func(_ context.Context, uid string) (bool, error) {
			require.Equal(t, project.UID, uid)
			return false, nil
		},
		Hub: func(context.Context, hybridParams, int) ([]api.FederationSearchHit, bool, error) {
			called = true
			return nil, false, nil
		},
	})
	var modeErr *modeError
	require.ErrorAs(t, err, &modeErr)
	require.Equal(t, http.StatusServiceUnavailable, modeErr.Status())
	require.False(t, called)
}

func TestEmbeddingProjectPolicyRetainedArtifacts(t *testing.T) {
	policyErr := errors.New("policy unavailable")
	for _, tc := range []struct {
		name string
		err  error
	}{{"denied", nil}, {"policy-error", policyErr}, {"stale", kitvec.ErrStale}} {
		name := tc.name
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store := newReconcilerTestStore(t)
			idx := openTestVectorIndex(t)
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "example task", Author: "tester"})
			require.NoError(t, err)
			client, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:1", Model: "m", Dims: 2})
			require.NoError(t, err)
			recipe, err := client.ArtifactIdentity("", "", store.InstanceUID())
			require.NoError(t, err)
			identity := recipe
			identity.ProjectUID, identity.IssueUID = project.UID, issue.UID
			artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
			require.NoError(t, err)
			_, err = store.RetainEmbeddingArtifact(ctx, artifact)
			require.NoError(t, err)
			key := client.Generation().Fingerprint()
			require.NoError(t, idx.EnsureBuilding(ctx, key, client.Generation()))
			deny := func(_ context.Context, uid string) (bool, error) {
				require.Equal(t, project.UID, uid)
				return false, tc.err
			}
			calls := 0
			r := NewReconciler(store, idx, client, ReconcilerConfig{EmbeddingProjectAllowed: func(ctx context.Context, uid string) (bool, error) {
				calls++
				if calls == 1 {
					return true, nil
				}
				return deny(ctx, uid)
			}})
			err = r.reconcileOnce(ctx)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			backlog, err := idx.Backlog(ctx, key)
			require.NoError(t, err)
			require.EqualValues(t, 1, backlog)
			hits, err := idx.Query(ctx, key, kitvec.Vector{1, 0}, 10)
			require.NoError(t, err)
			require.Empty(t, hits)
		})
	}
}
