package daemon_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// countingEmbedder fails the test's zero-call assertion by counting every
// document input the provider would receive.
type countingEmbedder struct{ inputs atomic.Int64 }

func (*countingEmbedder) Generation() kitvec.Generation {
	return kitvec.Generation{Model: "pg-replica", Dimensions: 2}
}

func (e *countingEmbedder) EncodeFunc() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		e.inputs.Add(int64(len(texts)))
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
}

// staticUpstream serves the unit vector [0, 1] for every requested document
// whose hash matches content.
type staticUpstream struct {
	generation *api.FederationVectorGeneration
	content    map[string]string
}

func (u staticUpstream) LookupVectors(_ context.Context, _ db.FederationBinding, _ string, req api.FederationVectorLookupRequestBody) (api.FederationVectorLookupBody, error) {
	body := api.FederationVectorLookupBody{Generation: u.generation}
	for _, d := range req.Docs {
		rec := api.FederationVectorRecord{IssueUID: d.IssueUID, Status: api.FederationVectorStatusNotReady}
		if text, ok := u.content[d.IssueUID]; ok && vector.ContentSHA256(text) == d.ContentSHA256 {
			rec.Status = api.FederationVectorStatusOK
			rec.ContentSHA256 = d.ContentSHA256
			rec.Chunks = []api.FederationVectorChunk{{Index: 0, Vector: vector.EncodeVector(kitvec.Vector{0, 1})}}
		}
		body.Records = append(body.Records, rec)
	}
	return body, nil
}

func TestPostgresReconcilerImportsReplicaVectors(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "replica")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "t", Body: "b", Author: "x"})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:7373",
		HubProjectID: 42, HubProjectUID: project.UID, ReplayHorizonEventID: 1, PullCursorEventID: 1,
		Actor: "x", Enabled: true,
	})
	require.NoError(t, err)
	idx, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	emb := &countingEmbedder{}
	gen := emb.Generation()
	up := staticUpstream{
		generation: &api.FederationVectorGeneration{Fingerprint: gen.Fingerprint(), Model: gen.Model, Dims: 2, State: "active"},
		content:    map[string]string{issue.UID: embedding.EmbedText("t", "b")},
	}
	r := daemon.NewReconciler(store, idx, emb, daemon.ReconcilerConfig{BatchSize: 64, SweepEvery: time.Hour, Upstream: up})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		h := r.Health()
		return h.Replicated == 1 && h.Backlog == 0 && h.LastSuccessAt != nil
	}, 10*time.Second, 20*time.Millisecond, "PostgreSQL spoke did not import the hub vector")
	assert.Zero(t, emb.inputs.Load(), "a replica row must never reach the provider")
	hits, err := idx.Query(ctx, gen.Fingerprint(), kitvec.Vector{0, 1}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, issue.UID, hits[0].Doc)
}

func TestPostgresReplicaUpgradeRetainsActiveGenerationUntilImport(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "replica")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "t", Body: "b", Author: "x"})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:7373",
		HubProjectID: 42, HubProjectUID: project.UID, ReplayHorizonEventID: 1, PullCursorEventID: 1,
		Actor: "x", Enabled: true,
	})
	require.NoError(t, err)
	idx, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	old := kitvec.Generation{Model: "old", Dimensions: 2}
	oldKey := old.Fingerprint()
	release, err := idx.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	require.NoError(t, idx.EnsureBuilding(ctx, oldKey, old))
	_, err = idx.Fill(ctx, oldKey, func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}, 64, nil, nil)
	require.NoError(t, err)
	require.NoError(t, idx.CutOver(ctx, oldKey))
	require.NoError(t, release())

	emb := &countingEmbedder{}
	gen := emb.Generation()
	newKey := gen.Fingerprint()
	runUntil := func(up staticUpstream, doneWhen func(daemon.ReconcilerHealth) bool) {
		runCtx, cancel := context.WithCancel(ctx)
		r := daemon.NewReconciler(store, idx, emb, daemon.ReconcilerConfig{
			BatchSize: 64, SweepEvery: time.Hour, Upstream: up,
		})
		done := make(chan error, 1)
		go func() { done <- r.Run(runCtx) }()
		require.Eventually(t, func() bool { return doneWhen(r.Health()) }, 10*time.Second, 20*time.Millisecond)
		cancel()
		<-done
	}
	baseUpstream := staticUpstream{
		generation: &api.FederationVectorGeneration{Fingerprint: newKey, Model: gen.Model, Dims: 2, State: "active"},
		content:    map[string]string{},
	}
	runUntil(baseUpstream, func(h daemon.ReconcilerHealth) bool {
		return h.LastSuccessAt != nil && h.Backlog == 1
	})
	active, ok, err := idx.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, oldKey, active)
	oldVectors, err := idx.LookupVectors(ctx, oldKey, project.UID, []string{issue.UID})
	require.NoError(t, err)
	assert.NotEmpty(t, oldVectors[issue.UID].Chunks)
	assert.Zero(t, emb.inputs.Load(), "pending replica text must not reach the provider")

	baseUpstream.content[issue.UID] = embedding.EmbedText("t", "b")
	runUntil(baseUpstream, func(h daemon.ReconcilerHealth) bool {
		return h.LastSuccessAt != nil && h.Replicated == 1 && h.Backlog == 0
	})
	active, ok, err = idx.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, newKey, active)
	state, err := idx.GenerationState(ctx, oldKey)
	require.NoError(t, err)
	assert.Equal(t, "retired", state)
}
