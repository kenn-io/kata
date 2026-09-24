package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// fakeHubDoc is what the fake hub holds for one issue: the text its vectors
// were computed from, and optional damage applied to the served chunks.
type fakeHubDoc struct {
	content string
	skipped bool
	corrupt func([]api.FederationVectorChunk) []api.FederationVectorChunk
}

// fakeUpstream plays the hub side of vectors:lookup. Its vectors point along
// axis 1, while fakeEmbedder's point along axis 0, so a test can tell an
// imported vector from a locally embedded one.
type fakeUpstream struct {
	mu         sync.Mutex
	generation *api.FederationVectorGeneration
	err        error
	docs       map[string]fakeHubDoc
	deferUID   string // answered "deferred" while set
	calls      []api.FederationVectorLookupRequestBody
}

func (f *fakeUpstream) LookupVectors(_ context.Context, _ db.FederationBinding, _ string, req api.FederationVectorLookupRequestBody) (api.FederationVectorLookupBody, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return api.FederationVectorLookupBody{}, f.err
	}
	body := api.FederationVectorLookupBody{Generation: f.generation, Records: []api.FederationVectorRecord{}}
	if f.generation == nil || req.Fingerprint != f.generation.Fingerprint {
		return body, nil
	}
	for _, d := range req.Docs {
		rec := api.FederationVectorRecord{IssueUID: d.IssueUID, Status: api.FederationVectorStatusNotReady}
		hub, ok := f.docs[d.IssueUID]
		if d.IssueUID == f.deferUID {
			rec.Status = api.FederationVectorStatusDeferred
		} else if ok && vector.ContentSHA256(hub.content) == d.ContentSHA256 {
			rec.ContentSHA256 = d.ContentSHA256
			rec.Status = api.FederationVectorStatusOK
			if hub.skipped {
				rec.Status = api.FederationVectorStatusSkipped
			} else {
				for _, c := range kitvec.Split(hub.content, embedding.SplitOptions()) {
					v := make(kitvec.Vector, f.generation.Dims)
					v[1] = 1
					rec.Chunks = append(rec.Chunks, api.FederationVectorChunk{Index: c.Index, Vector: vector.EncodeVector(v)})
				}
				if hub.corrupt != nil {
					rec.Chunks = hub.corrupt(rec.Chunks)
				}
			}
		}
		body.Records = append(body.Records, rec)
	}
	return body, nil
}

func (f *fakeUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeUpstream) setDoc(uid string, doc fakeHubDoc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[uid] = doc
}

func generationDescriptor(g kitvec.Generation) *api.FederationVectorGeneration {
	return &api.FederationVectorGeneration{
		Fingerprint: g.Fingerprint(), Model: g.Model, Dims: g.Dimensions, Params: g.Params, State: "active",
	}
}

// replicaFixture is a spoke store with one local-only issue and two issues in
// a federated replica project, a fake hub that has embedded both replica
// issues, and a reconciler on a controllable clock.
type replicaFixture struct {
	ctx      context.Context
	store    *sqlitestore.Store
	idx      *vector.Index
	emb      *fakeEmbedder
	up       *fakeUpstream
	r        *Reconciler
	now      time.Time
	local    db.Project
	replica  db.Project
	issues   []db.Issue // the two replica issues
	localUID string
}

func newReplicaFixture(t *testing.T) *replicaFixture {
	t.Helper()
	f := &replicaFixture{ctx: context.Background(), now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	f.store = newReconcilerTestStore(t)
	var err error
	f.local, err = f.store.CreateProject(f.ctx, "local")
	require.NoError(t, err)
	localIssue, _, err := f.store.CreateIssue(f.ctx, db.CreateIssueParams{ProjectID: f.local.ID, Title: "local", Body: "only here", Author: "x"})
	require.NoError(t, err)
	f.localUID = localIssue.UID
	f.replica, err = f.store.CreateProject(f.ctx, "replica")
	require.NoError(t, err)
	for _, title := range []string{"login loop", "slow dashboard"} {
		issue, _, err := f.store.CreateIssue(f.ctx, db.CreateIssueParams{ProjectID: f.replica.ID, Title: title, Body: "body", Author: "x"})
		require.NoError(t, err)
		f.issues = append(f.issues, issue)
	}
	_, err = f.store.UpsertFederationBinding(f.ctx, db.FederationBinding{
		ProjectID: f.replica.ID, Role: db.FederationRoleSpoke,
		HubURL: "http://127.0.0.1:7373", HubProjectID: 42, HubProjectUID: f.replica.UID,
		ReplayHorizonEventID: 1, PullCursorEventID: 1, Actor: "x",
		PushEnabled: true, // lets the test edit replica issues locally
		Enabled:     true,
	})
	require.NoError(t, err)
	f.idx = openTestVectorIndex(t)
	f.emb = &fakeEmbedder{model: "m1", dims: 4}
	f.up = &fakeUpstream{generation: generationDescriptor(f.emb.Generation()), docs: map[string]fakeHubDoc{}}
	for _, issue := range f.issues {
		f.up.docs[issue.UID] = fakeHubDoc{content: embedding.EmbedText(issue.Title, issue.Body)}
	}
	f.r = NewReconciler(f.store, f.idx, f.emb, ReconcilerConfig{
		BatchSize: 64, Upstream: f.up, Now: func() time.Time { return f.now },
	})
	return f
}

func (f *replicaFixture) reconcile(t *testing.T) {
	t.Helper()
	require.NoError(t, f.r.reconcileOnce(f.ctx))
}

func (f *replicaFixture) backlog(t *testing.T) int64 {
	t.Helper()
	n, err := f.idx.Backlog(f.ctx, f.emb.Generation().Fingerprint())
	require.NoError(t, err)
	return n
}

// topDoc returns the best-scoring document for a query along axis.
func (f *replicaFixture) topDoc(t *testing.T, axis int) string {
	t.Helper()
	q := make(kitvec.Vector, 4)
	q[axis] = 1
	hits, err := f.idx.Query(f.ctx, f.emb.Generation().Fingerprint(), q, 10)
	require.NoError(t, err)
	rolled := kitvec.RollupByDocument(hits)
	require.NotEmpty(t, rolled)
	return rolled[0].Doc
}

// Acceptance 1 and the zero-provider-call guarantee: with a compatible hub,
// the default configuration embeds only the local-only issue.
func TestReplicaImportMakesNoProviderCallsForFederatedRows(t *testing.T) {
	f := newReplicaFixture(t)
	f.reconcile(t)

	assert.Equal(t, 1, f.emb.n, "only the local-only issue may reach the provider")
	assert.Zero(t, f.backlog(t))
	h := f.r.Health()
	assert.Equal(t, "replica", h.Source)
	assert.Equal(t, ReplicaStatusOK, h.SourceStatus)
	assert.Equal(t, int64(2), h.Replicated)
	assert.Zero(t, h.AwaitingUpstream)
	require.NotNil(t, h.LastReplicaSuccessAt)
	require.Len(t, h.ReplicaProjects, 1)
	assert.Equal(t, f.replica.UID, h.ReplicaProjects[0].ProjectUID)
	assert.Equal(t, f.emb.Generation().Fingerprint(), h.ReplicaProjects[0].UpstreamFingerprint)
	assert.NotEqual(t, f.localUID, f.topDoc(t, 1), "axis-1 vectors must come from the hub")
	assert.Equal(t, f.localUID, f.topDoc(t, 0))
}

func TestReplicaImportRebuildsNilProjectState(t *testing.T) {
	f := newReplicaFixture(t)
	f.r.replica.projects = map[string]*replicaProject{f.replica.UID: nil}

	f.reconcile(t)

	assert.Equal(t, 1, f.emb.n, "only the local-only issue may reach the provider")
	assert.Zero(t, f.backlog(t))
	assert.Equal(t, ReplicaStatusOK, f.r.Health().ReplicaProjects[0].Status)
}

// Acceptance 2: a local edit changes the hash, so the row waits (no provider
// call, no repeated lookups inside the backoff) until the hub re-embeds.
func TestReplicaImportWaitsForHubAfterLocalEdit(t *testing.T) {
	f := newReplicaFixture(t)
	f.reconcile(t)
	target := f.issues[0]
	title := "login loop after edit"
	_, _, _, err := f.store.EditIssue(f.ctx, db.EditIssueParams{IssueID: target.ID, Title: &title, Actor: "x"})
	require.NoError(t, err)

	f.reconcile(t)
	assert.Equal(t, 1, f.emb.n, "an edited replica row must not be embedded locally")
	assert.Equal(t, int64(1), f.backlog(t))
	assert.Equal(t, int64(1), f.r.Health().AwaitingUpstream)
	assert.Equal(t, 30*time.Second, f.r.successDelay(), "the reconciler must wake for the retry")

	calls := f.up.callCount()
	f.now = f.now.Add(10 * time.Second)
	f.reconcile(t)
	assert.Equal(t, calls, f.up.callCount(), "a backing-off row must not be looked up again")

	f.up.setDoc(target.UID, fakeHubDoc{content: embedding.EmbedText(title, target.Body)}) // hub re-embedded
	f.now = f.now.Add(25 * time.Second)
	f.reconcile(t)
	assert.Zero(t, f.backlog(t))
	h := f.r.Health()
	assert.Equal(t, int64(3), h.Replicated)
	assert.Zero(t, h.AwaitingUpstream)
	assert.Equal(t, 1, f.emb.n)
}

// Acceptance 3 and 4: a hub that cannot supply compatible vectors leaves the
// project embedding locally, with source_status saying why.
func TestReplicaImportFallsBackToLocalEmbedding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*fakeUpstream)
		status string
	}{
		{"generation mismatch", func(u *fakeUpstream) {
			u.generation = generationDescriptor(kitvec.Generation{Model: "other", Dimensions: 4})
		}, ReplicaStatusGenerationMismatch},
		{"older hub", func(u *fakeUpstream) {
			u.err = fmt.Errorf("%w: hub returned 404", ErrVectorUpstreamUnsupported)
		}, ReplicaStatusUnsupported},
		{"hub without embeddings", func(u *fakeUpstream) { u.generation = nil }, ReplicaStatusNoPublisher},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplicaFixture(t)
			tc.setup(f.up)
			f.reconcile(t)

			h := f.r.Health()
			assert.Equal(t, tc.status, h.SourceStatus)
			assert.Zero(t, h.Replicated, "nothing may be imported")
			assert.Equal(t, 3, f.emb.n, "every row falls back to the provider")
			assert.Zero(t, f.backlog(t))
			if tc.status == ReplicaStatusGenerationMismatch {
				assert.Equal(t, f.up.generation.Fingerprint, h.ReplicaProjects[0].UpstreamFingerprint)
			}
			calls := f.up.callCount()
			f.now = f.now.Add(10 * time.Minute)
			f.reconcile(t)
			assert.Equal(t, calls, f.up.callCount(), "no re-probe before replicaReprobeEvery")
		})
	}
}

func TestReplicaImportUnreachableHubKeepsRowsPending(t *testing.T) {
	f := newReplicaFixture(t)
	f.up.err = errors.New("dial tcp: connection refused")
	f.reconcile(t)

	h := f.r.Health()
	assert.Equal(t, ReplicaStatusUnreachable, h.SourceStatus)
	assert.Equal(t, 1, f.emb.n, "an unreachable hub must not trigger provider calls")
	assert.Equal(t, int64(2), f.backlog(t))
	assert.Nil(t, h.LastReplicaSuccessAt)

	calls := f.up.callCount()
	f.reconcile(t)
	assert.Equal(t, calls, f.up.callCount(), "the project backs off after a failure")

	f.up.mu.Lock()
	f.up.err = nil
	f.up.mu.Unlock()
	f.now = f.now.Add(31 * time.Second)
	f.reconcile(t)
	assert.Equal(t, ReplicaStatusOK, f.r.Health().SourceStatus)
	assert.Zero(t, f.backlog(t))
}

// Acceptance 5: malformed hub vectors are counted and never reach the index.
func TestReplicaImportRejectsMalformedVectors(t *testing.T) {
	nan := float32(math.NaN())
	for name, corrupt := range map[string]func([]api.FederationVectorChunk) []api.FederationVectorChunk{
		"wrong dims": func(c []api.FederationVectorChunk) []api.FederationVectorChunk {
			c[0].Vector = vector.EncodeVector(kitvec.Vector{0, 1})
			return c
		},
		"not finite": func(c []api.FederationVectorChunk) []api.FederationVectorChunk {
			c[0].Vector = vector.EncodeVector(kitvec.Vector{nan, 1, 0, 0})
			return c
		},
		"not normalized": func(c []api.FederationVectorChunk) []api.FederationVectorChunk {
			c[0].Vector = vector.EncodeVector(kitvec.Vector{0, 2, 0, 0})
			return c
		},
		"missing chunks": func([]api.FederationVectorChunk) []api.FederationVectorChunk { return nil },
		"extra chunk": func(c []api.FederationVectorChunk) []api.FederationVectorChunk {
			return append(c, api.FederationVectorChunk{Index: 1, Vector: c[0].Vector})
		},
		"wrong index": func(c []api.FederationVectorChunk) []api.FederationVectorChunk {
			c[0].Index = 7
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newReplicaFixture(t)
			bad := f.issues[0]
			f.up.docs[bad.UID] = fakeHubDoc{content: embedding.EmbedText(bad.Title, bad.Body), corrupt: corrupt}
			f.reconcile(t)

			h := f.r.Health()
			assert.Equal(t, int64(1), h.Rejected)
			assert.Equal(t, int64(1), h.Replicated, "the valid sibling still imports")
			assert.Equal(t, int64(1), f.backlog(t), "the rejected row stays pending")
			assert.Equal(t, 1, f.emb.n, "a rejected row is not embedded locally either")
			assert.Equal(t, int64(1), h.AwaitingUpstream)
		})
	}
}

func TestReplicaImportStampsSkippedDocsWithoutVectors(t *testing.T) {
	f := newReplicaFixture(t)
	skipped := f.issues[1]
	f.up.docs[skipped.UID] = fakeHubDoc{content: embedding.EmbedText(skipped.Title, skipped.Body), skipped: true}
	f.reconcile(t)

	embedded, skippedCount, backlog, err := f.idx.Coverage(f.ctx, f.emb.Generation().Fingerprint())
	require.NoError(t, err)
	assert.Equal(t, int64(2), embedded) // local + one replica
	assert.Equal(t, int64(1), skippedCount)
	assert.Zero(t, backlog)
	assert.Equal(t, int64(2), f.r.Health().Replicated)
}

// Acceptance 6: a daemon with no federated replica projects behaves as
// before and never contacts an upstream.
func TestReconcilerWithoutReplicaProjectsIsUnchanged(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "standalone")
	require.NoError(t, err)
	for range 3 {
		_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"})
		require.NoError(t, err)
	}
	emb := &fakeEmbedder{model: "m1", dims: 4}
	up := &fakeUpstream{generation: generationDescriptor(emb.Generation()), docs: map[string]fakeHubDoc{}}
	r := NewReconciler(store, openTestVectorIndex(t), emb, ReconcilerConfig{BatchSize: 64, Upstream: up})
	require.NoError(t, r.reconcileOnce(ctx))

	assert.Equal(t, 3, emb.n)
	assert.Zero(t, up.callCount())
	h := r.Health()
	assert.Equal(t, "provider", h.Source)
	assert.Equal(t, ReplicaStatusDisabled, h.SourceStatus)
	assert.Empty(t, h.ReplicaProjects)
}

func TestReconcilerWithoutUpstreamEmbedsReplicaRowsLocally(t *testing.T) {
	f := newReplicaFixture(t)
	f.r.cfg.Upstream = nil
	f.reconcile(t)
	assert.Equal(t, 3, f.emb.n)
	assert.Equal(t, ReplicaStatusDisabled, f.r.Health().SourceStatus)
}

func TestReplicaBackoffDoublesToCap(t *testing.T) {
	assert.Equal(t, 30*time.Second, replicaBackoff(1))
	assert.Equal(t, time.Minute, replicaBackoff(2))
	assert.Equal(t, 16*time.Minute, replicaBackoff(6))
	assert.Equal(t, 30*time.Minute, replicaBackoff(7))
	assert.Equal(t, 30*time.Minute, replicaBackoff(1000))
}

// Review focus: a daemon that hubs one project and replicates another embeds
// its hub-role project locally.
func TestReplicaImportIgnoresHubRoleBindings(t *testing.T) {
	f := newReplicaFixture(t)
	_, err := f.store.EnableProjectFederation(f.ctx, f.local.ID, "x")
	require.NoError(t, err)
	f.reconcile(t)
	assert.Equal(t, 1, f.emb.n, "the hub-role project's issue is embedded locally")
	require.Len(t, f.r.Health().ReplicaProjects, 1)
	assert.Equal(t, f.replica.UID, f.r.Health().ReplicaProjects[0].ProjectUID)
}

// Review focus: leaving federation turns a replica project back into a local
// one; its still-pending rows are embedded on the next turn.
func TestReplicaImportStopsWhenBindingDisabled(t *testing.T) {
	f := newReplicaFixture(t)
	f.up.err = errors.New("dial tcp: connection refused")
	f.reconcile(t)
	require.Equal(t, int64(2), f.backlog(t))

	binding, err := f.store.FederationBindingByProject(f.ctx, f.replica.ID)
	require.NoError(t, err)
	binding.Enabled = false
	_, err = f.store.UpsertFederationBinding(f.ctx, binding)
	require.NoError(t, err)
	f.reconcile(t)

	assert.Zero(t, f.backlog(t))
	assert.Equal(t, 3, f.emb.n)
	assert.Equal(t, ReplicaStatusDisabled, f.r.Health().SourceStatus)
}

// Review focus: long issues replicate chunk-for-chunk.
func TestReplicaImportAcceptsMultiChunkDocuments(t *testing.T) {
	f := newReplicaFixture(t)
	long := f.issues[0]
	body := strings.Repeat("the oauth callback drops the session cookie ", 250) // ~11k runes
	_, _, _, err := f.store.EditIssue(f.ctx, db.EditIssueParams{IssueID: long.ID, Body: &body, Actor: "x"})
	require.NoError(t, err)
	content := embedding.EmbedText(long.Title, body)
	require.Greater(t, len(kitvec.Split(content, embedding.SplitOptions())), 4)
	f.up.docs[long.UID] = fakeHubDoc{content: content}
	f.reconcile(t)

	assert.Zero(t, f.backlog(t))
	assert.Zero(t, f.r.Health().Rejected)
	assert.Equal(t, 1, f.emb.n)
}

// Review focus: a record the hub deferred for its response budget is asked
// for again promptly, without the not_ready backoff.
func TestReplicaImportRetriesDeferredWithoutBackoff(t *testing.T) {
	f := newReplicaFixture(t)
	deferred := f.issues[1]
	f.up.docs[deferred.UID] = fakeHubDoc{content: embedding.EmbedText(deferred.Title, deferred.Body),
		corrupt: func([]api.FederationVectorChunk) []api.FederationVectorChunk { return nil }}
	f.up.deferUID = deferred.UID
	f.reconcile(t)
	assert.Equal(t, int64(1), f.backlog(t))
	assert.Zero(t, f.r.Health().AwaitingUpstream, "deferred is not a failure")
	assert.Equal(t, f.r.cfg.MinBackoff, f.r.successDelay())

	f.up.mu.Lock()
	f.up.deferUID = ""
	f.up.docs[deferred.UID] = fakeHubDoc{content: embedding.EmbedText(deferred.Title, deferred.Body)}
	f.up.mu.Unlock()
	f.reconcile(t)
	assert.Zero(t, f.backlog(t))
}

func TestReplicaRetryDelayRespectsProjectBackoff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fakeUpstream)
		status string
	}{
		{"unreachable", func(u *fakeUpstream) { u.err = errors.New("hub unavailable") }, ReplicaStatusUnreachable},
		{"unsupported", func(u *fakeUpstream) { u.err = ErrVectorUpstreamUnsupported }, ReplicaStatusUnsupported},
		{"no publisher", func(u *fakeUpstream) { u.generation = nil }, ReplicaStatusNoPublisher},
		{"generation mismatch", func(u *fakeUpstream) {
			u.generation = generationDescriptor(kitvec.Generation{Model: "other", Dimensions: 4})
		}, ReplicaStatusGenerationMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplicaFixture(t)
			delete(f.up.docs, f.issues[0].UID)
			f.reconcile(t) // one document receives not_ready and a 30s retry
			require.Equal(t, int64(1), f.backlog(t))
			f.now = f.now.Add(31 * time.Second)
			tc.change(f.up)
			f.reconcile(t)
			assert.Equal(t, tc.status, f.r.Health().SourceStatus)
			assert.Greater(t, f.r.successDelay(), time.Duration(0), "expired document retry must not spin the reconciler")
			calls := f.up.callCount()
			f.now = f.now.Add(time.Second)
			f.reconcile(t)
			assert.Equal(t, calls, f.up.callCount(), "project backoff must suppress repeated lookups")
		})
	}
}

func TestReplicaUpgradeKeepsActiveGenerationUntilImportsComplete(t *testing.T) {
	f := newReplicaFixture(t)
	f.reconcile(t)
	oldKey := f.emb.Generation().Fingerprint()
	target := f.issues[0]
	f.emb.model = "m2"
	f.up.generation = generationDescriptor(f.emb.Generation())
	delete(f.up.docs, target.UID)
	f.reconcile(t)
	newKey := f.emb.Generation().Fingerprint()
	active, ok, err := f.idx.ActiveGeneration(f.ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, oldKey, active, "the complete generation must keep serving while a replica row waits")
	oldVectors, err := f.idx.LookupVectors(f.ctx, oldKey, f.replica.UID, []string{target.UID})
	require.NoError(t, err)
	assert.NotEmpty(t, oldVectors[target.UID].Chunks, "old vectors must not be reclaimed early")
	assert.Equal(t, int64(1), f.backlog(t))

	f.up.setDoc(target.UID, fakeHubDoc{content: embedding.EmbedText(target.Title, target.Body)})
	f.now = f.now.Add(31 * time.Second)
	f.reconcile(t)
	active, ok, err = f.idx.ActiveGeneration(f.ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, newKey, active)
	assert.Zero(t, f.backlog(t))
	state, err := f.idx.GenerationState(f.ctx, oldKey)
	require.NoError(t, err)
	assert.Equal(t, "retired", state)
}
