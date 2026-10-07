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

// Exercise the runner and real fetcher together: a filtered initial import must
// never walk the repository-wide parent connection or historical event feed.
func TestRunnerFilteredParentBootstrap(t *testing.T) {
	for _, mode := range []string{"fresh", "empty", "partial-retry", "finalized-retry", "parent-failure", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			h := newRunnerHarness(t, withInitialBatchSize(1))
			cutoff := h.now.Add(-time.Hour)
			setParentRunnerConfig(t, h, false, &cutoff)
			issues := []Issue{
				testIssue(101, 1, "child", h.now),
				testIssue(102, 2, "parent", h.now),
				testIssue(103, 3, "old", cutoff.Add(-time.Second)),
				testIssue(104, 4, "boundary", cutoff),
				testIssue(105, 5, "pull", h.now),
			}
			issues[4].PullRequest = &PullRequest{}
			wantNumbers := []int{1, 2}
			if mode == "empty" {
				issues = issues[2:]
				wantNumbers = nil
			}
			parentFailure := mode == "parent-failure"
			var phases []string
			var selections [][]int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/example-owner/example-repo":
					_, _ = fmt.Fprint(w, `{"node_id":"R_example_repo","id":101,"full_name":"example-owner/example-repo"}`)
				case "/repos/example-owner/example-repo/issues":
					phases = append(phases, "issues")
					assert.Equal(t, cutoff.Format(time.RFC3339), r.URL.Query().Get("since"))
					require.NoError(t, json.MarshalWrite(w, issues))
				case "/graphql":
					phases = append(phases, "parents")
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
					selections = append(selections, selected)
					assert.Equal(t, wantNumbers, selected)
					if parentFailure {
						http.Error(w, "parent request failed", http.StatusBadRequest)
						return
					}
					if mode == "unsupported" {
						_, _ = fmt.Fprint(w, `{"errors":[{"message":"Field 'parent' doesn't exist on type 'Issue'","type":"undefinedField","path":["query","repository","i0","parent"]}]}`)
						return
					}
					require.NoError(t, json.MarshalWrite(w, map[string]any{"data": map[string]any{"repository": nodes}}))
				default:
					t.Errorf("unexpected bootstrap request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			f := newParentGraphQLTestFetcher(server.URL + "/graphql")
			f.restBaseURLOverride = server.URL + "/"
			h.runner.config.Fetcher = f

			if mode == "partial-retry" {
				h.store.failImportCall = 2
				h.store.importErr = errors.New("later import batch failed")
			}
			var successStore *bootstrapSuccessFailureStore
			if mode == "finalized-retry" {
				successStore = &bootstrapSuccessFailureStore{Storage: h.store, fail: true}
				h.runner.config.Store = successStore
			}
			result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
			if mode == "partial-retry" || mode == "finalized-retry" || mode == "parent-failure" {
				require.Error(t, err)
				stored, lookupErr := h.db.IssueSyncBindingByID(h.ctx, h.binding.ID)
				require.NoError(t, lookupErr)
				require.Nil(t, stored.LastCursorAt)
				cfg, lookupErr := DecodeConfig(stored.Config)
				require.NoError(t, lookupErr)
				assert.Equal(t, mode != "finalized-retry", cfg.NeedsParentLinkBackfill())
				h.store.failImportCall = 0
				if successStore != nil {
					successStore.fail = false
				}
				parentFailure = false
				h.advance(time.Minute)
				result, err = h.runner.RunOnce(h.ctx, h.binding.ID)
			}
			require.NoError(t, err)
			cfg, err := DecodeConfig(result.Binding.Config)
			require.NoError(t, err)
			assert.Equal(t, mode == "unsupported", cfg.NeedsParentLinkBackfill())
			assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
			if mode == "empty" {
				assert.Empty(t, selections)
				assert.Equal(t, 0, result.Import.Created)
			} else {
				require.GreaterOrEqual(t, len(phases), 2)
				assert.Equal(t, []string{"issues", "parents"}, phases[:2])
				if mode != "unsupported" {
					assertSourceParent(t, h, "issue-id:101", "issue-id:102")
				}
			}
			for _, id := range []string{"issue-id:103", "issue-id:104", "issue-id:105"} {
				_, err := h.db.ImportMappingBySource(h.ctx, h.project.ID, h.binding.SourceKey, "issue", id)
				assert.ErrorIs(t, err, db.ErrNotFound)
			}
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
