package githubsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// filteredBootstrap serves a filtered initial import through the real fetcher.
// It records request phases and parent selections so tests can prove the run
// never walks the repository-wide parent connection or historical event feed.
type filteredBootstrap struct {
	h           *runnerHarness
	cutoff      time.Time
	issues      []Issue
	wantNumbers []int
	parentReply string
	phases      []string
	selections  [][]int
}

const (
	parentReplyFailure     = "failure"
	parentReplyUnsupported = "unsupported"
)

func newFilteredBootstrap(t *testing.T) *filteredBootstrap {
	t.Helper()
	h := newRunnerHarness(t, withInitialBatchSize(1))
	b := &filteredBootstrap{h: h, cutoff: h.now.Add(-time.Hour), wantNumbers: []int{1, 2}}
	setParentRunnerConfig(t, h, false, &b.cutoff)
	b.issues = []Issue{
		testIssue(101, 1, "child", h.now),
		testIssue(102, 2, "parent", h.now),
		testIssue(103, 3, "old", b.cutoff.Add(-time.Second)),
		testIssue(104, 4, "boundary", b.cutoff),
		testIssue(105, 5, "pull", h.now),
	}
	b.issues[4].PullRequest = &PullRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.serve(t, w, r)
	}))
	t.Cleanup(server.Close)
	f := newParentGraphQLTestFetcher(server.URL + "/graphql")
	f.restBaseURLOverride = server.URL + "/"
	h.runner.config.Fetcher = f
	return b
}

func (b *filteredBootstrap) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/repos/example-owner/example-repo":
		_, _ = fmt.Fprint(w, `{"node_id":"R_example_repo","id":101,"full_name":"example-owner/example-repo"}`)
	case "/repos/example-owner/example-repo/issues":
		b.phases = append(b.phases, "issues")
		assert.Equal(t, b.cutoff.Format(time.RFC3339), r.URL.Query().Get("since"))
		require.NoError(t, json.MarshalWrite(w, b.issues))
	case "/graphql":
		b.phases = append(b.phases, "parents")
		b.serveParents(t, w, r)
	default:
		t.Errorf("unexpected bootstrap request: %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func (b *filteredBootstrap) serveParents(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var request parentGraphQLRequest
	require.NoError(t, json.UnmarshalRead(r.Body, &request))
	assert.NotContains(t, request.Query, "issues(first:")
	var selected []int
	nodes := map[string]any{}
	for _, match := range testParentAlias.FindAllStringSubmatch(request.Query, -1) {
		n, err := strconv.Atoi(match[2])
		require.NoError(t, err)
		selected = append(selected, n)
		node := map[string]any{"number": n, "fullDatabaseId": 100 + n, "parent": nil}
		if n == 1 {
			node["parent"] = map[string]any{"number": 2, "fullDatabaseId": 102}
		}
		nodes["i"+match[1]] = node
	}
	b.selections = append(b.selections, selected)
	assert.Equal(t, b.wantNumbers, selected)
	switch b.parentReply {
	case parentReplyFailure:
		http.Error(w, "parent request failed", http.StatusBadRequest)
	case parentReplyUnsupported:
		_, _ = fmt.Fprint(w, `{"errors":[{"message":"Field 'parent' doesn't exist on type 'Issue'","type":"undefinedField","path":["query","repository","i0","parent"]}]}`)
	default:
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": map[string]any{"repository": nodes}}))
	}
}

// assertCompleted checks the state every successful filtered bootstrap leaves:
// a stored cursor, the backfill marker, and no imports outside the cutoff.
func (b *filteredBootstrap) assertCompleted(t *testing.T, result RunResult, wantBackfillPending bool) {
	t.Helper()
	cfg, err := DecodeConfig(result.Binding.Config)
	require.NoError(t, err)
	assert.Equal(t, wantBackfillPending, cfg.NeedsParentLinkBackfill())
	assertCursorAt(b.h.ctx, t, b.h.db, b.h.binding.ID, b.h.now)
	for _, id := range []string{"issue-id:103", "issue-id:104", "issue-id:105"} {
		_, err := b.h.db.ImportMappingBySource(b.h.ctx, b.h.project.ID, b.h.binding.SourceKey, "issue", id)
		assert.ErrorIs(t, err, db.ErrNotFound)
	}
}

func (b *filteredBootstrap) assertIssuesBeforeParents(t *testing.T) {
	t.Helper()
	require.GreaterOrEqual(t, len(b.phases), 2)
	assert.Equal(t, []string{"issues", "parents"}, b.phases[:2])
}

func TestRunnerFilteredParentBootstrap(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		b := newFilteredBootstrap(t)
		result, err := b.h.runner.RunOnce(b.h.ctx, b.h.binding.ID)
		require.NoError(t, err)
		b.assertCompleted(t, result, false)
		b.assertIssuesBeforeParents(t)
		assertSourceParent(t, b.h, "issue-id:101", "issue-id:102")
	})
	t.Run("empty", func(t *testing.T) {
		b := newFilteredBootstrap(t)
		b.issues = b.issues[2:]
		b.wantNumbers = nil
		result, err := b.h.runner.RunOnce(b.h.ctx, b.h.binding.ID)
		require.NoError(t, err)
		b.assertCompleted(t, result, false)
		assert.Empty(t, b.selections)
		assert.Equal(t, 0, result.Import.Created)
	})
	t.Run("unsupported", func(t *testing.T) {
		b := newFilteredBootstrap(t)
		b.parentReply = parentReplyUnsupported
		result, err := b.h.runner.RunOnce(b.h.ctx, b.h.binding.ID)
		require.NoError(t, err)
		b.assertCompleted(t, result, true)
		b.assertIssuesBeforeParents(t)
	})
}

// A failed filtered bootstrap leaves the cursor unset. Its retry must keep the
// scoped selection, because every issue the failed attempt imported is still
// eligible.
func TestRunnerFilteredParentBootstrapRetry(t *testing.T) {
	cases := []struct {
		name string
		// fail makes the first run fail and returns a function that clears it.
		fail                func(*filteredBootstrap) func()
		wantBackfillPending bool
	}{
		{
			name: "partial-import",
			fail: func(b *filteredBootstrap) func() {
				b.h.store.failImportCall = 2
				b.h.store.importErr = errors.New("later import batch failed")
				return func() { b.h.store.failImportCall = 0 }
			},
			wantBackfillPending: true,
		},
		{
			name: "success-recording",
			fail: func(b *filteredBootstrap) func() {
				store := &bootstrapSuccessFailureStore{Storage: b.h.store, fail: true}
				b.h.runner.config.Store = store
				return func() { store.fail = false }
			},
		},
		{
			name: "parent-request",
			fail: func(b *filteredBootstrap) func() {
				b.parentReply = parentReplyFailure
				return func() { b.parentReply = "" }
			},
			wantBackfillPending: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newFilteredBootstrap(t)
			restore := tc.fail(b)
			_, err := b.h.runner.RunOnce(b.h.ctx, b.h.binding.ID)
			require.Error(t, err)
			stored, err := b.h.db.IssueSyncBindingByID(b.h.ctx, b.h.binding.ID)
			require.NoError(t, err)
			require.Nil(t, stored.LastCursorAt)
			cfg, err := DecodeConfig(stored.Config)
			require.NoError(t, err)
			assert.Equal(t, tc.wantBackfillPending, cfg.NeedsParentLinkBackfill())

			restore()
			b.h.advance(time.Minute)
			result, err := b.h.runner.RunOnce(b.h.ctx, b.h.binding.ID)
			require.NoError(t, err)
			b.assertCompleted(t, result, false)
			b.assertIssuesBeforeParents(t)
			assertSourceParent(t, b.h, "issue-id:101", "issue-id:102")
		})
	}
}

type bootstrapSuccessFailureStore struct {
	db.Storage
	fail bool
}

func (s *bootstrapSuccessFailureStore) RecordIssueSyncSuccess(ctx context.Context, p db.IssueSyncSuccessParams) (db.IssueSyncStatus, error) {
	if s.fail {
		return db.IssueSyncStatus{}, errors.New("success recording failed")
	}
	return s.Storage.RecordIssueSyncSuccess(ctx, p)
}

func TestRunnerFilteredParentBootstrapPreservesOutsideSelectionMappings(t *testing.T) {
	for _, priorSuccess := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior-success-%t", priorSuccess), func(t *testing.T) {
			h := newRunnerHarness(t)
			seedSourceParentLink(t, h, h.now.Add(-2*time.Hour))
			if priorSuccess {
				recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, h.now.Add(-time.Minute))
			}
			cfg, err := DecodeConfig(h.binding.Config)
			require.NoError(t, err)
			cutoff := h.now.Add(-time.Hour)
			cfg.Since = cutoff.Format(time.RFC3339)
			raw, err := EncodeConfig(cfg)
			require.NoError(t, err)
			binding, err := h.db.UpsertIssueSyncBinding(h.ctx, db.UpsertIssueSyncBindingParams{ProjectID: h.project.ID, Provider: "github", SourceKey: h.binding.SourceKey, RemoteID: h.binding.RemoteID, DisplayName: h.binding.DisplayName, Config: raw, IntervalSeconds: 300})
			require.NoError(t, err)
			require.Nil(t, binding.LastCursorAt)
			h.fetcher.issues = []Issue{testIssue(102, 2, "parent", h.now)}
			h.fetcher.parentDataSet = true
			h.fetcher.parentData = ParentData{Scan: ParentScanComplete, ScannedChildIDs: map[int]int64{1: 101, 2: 102}}
			_, err = h.runner.RunOnce(h.ctx, h.binding.ID)
			require.NoError(t, err)
			require.Len(t, h.fetcher.parentRequests, 1)
			assert.Nil(t, h.fetcher.parentRequests[0].Since)
			assert.Nil(t, h.fetcher.parentRequests[0].IssueNumbers, "old mapped child needs full parent coverage")
			assertNoParent(t, h, "issue-id:101")
		})
	}
}

// Legacy identities must not force a full scan when their canonical issues are
// selected; the importer adopts these aliases on the same pass.
func TestRunnerFilteredParentBootstrapCoversLegacyMappings(t *testing.T) {
	h := newRunnerHarness(t)
	cutoff := h.now.Add(-time.Hour)
	setParentRunnerConfig(t, h, false, &cutoff)
	issue := testIssue(101, 1, "child", h.now)
	legacy := issue
	legacy.ID = 0
	batch := BuildImportBatch(h.binding.SourceKey, []Issue{legacy}, nil, h.now)
	batch.ProjectID = h.project.ID
	_, _, err := h.db.ImportBatch(h.ctx, batch)
	require.NoError(t, err)
	h.fetcher.issues = []Issue{issue}
	h.fetcher.parentDataSet = true
	h.fetcher.parentData = ParentData{Scan: ParentScanComplete, ScannedChildIDs: map[int]int64{1: 101}}
	_, err = h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)
	require.Len(t, h.fetcher.parentRequests, 1)
	assert.Equal(t, []int{1}, h.fetcher.parentRequests[0].IssueNumbers)
	mapping, err := h.db.ImportMappingBySource(h.ctx, h.project.ID, h.binding.SourceKey, "issue", "issue-id:101")
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
}
