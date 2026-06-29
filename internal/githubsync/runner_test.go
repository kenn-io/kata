package githubsync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRunnerFirstSyncFetchesAllImportsCommentsEmitsEventsAndAdvancesCursor(t *testing.T) {
	h := newRunnerHarness(t)
	issueTime := h.now.Add(-time.Hour)
	h.fetcher.issues = []Issue{
		testIssue(101, 1, "first issue", issueTime),
		testIssue(102, 2, "second issue", issueTime.Add(time.Minute)),
	}
	h.fetcher.issues[0].Comments = 1
	h.fetcher.issues[1].Comments = 1
	h.fetcher.comments = map[int][]Comment{
		1: {{ID: 1001, NodeID: "C_first_1", Body: "first comment", User: &User{Login: "commenter"}, CreatedAt: ptrTime(issueTime.Add(time.Minute))}},
		2: {{ID: 1002, NodeID: "C_second_1", Body: "second comment", User: &User{Login: "commenter"}, CreatedAt: ptrTime(issueTime.Add(2 * time.Minute))}},
	}

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	require.Len(t, h.fetcher.issueCalls, 1)
	assert.Nil(t, h.fetcher.issueCalls[0].since)
	assert.Equal(t, []int{1, 2}, h.fetcher.commentCalls)
	assert.Equal(t, 2, result.Import.Created)
	assert.Equal(t, 2, result.Import.Comments)
	assert.Equal(t, 2, result.Status.LastCreated)
	assert.Equal(t, 2, result.Status.LastComments)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
	require.NotNil(t, result.Binding.LastCursorAt)
	assert.Equal(t, h.now, *result.Binding.LastCursorAt)
	require.NotNil(t, h.store.lastImportGuard)
	assert.Equal(t, h.binding.ID, h.store.lastImportGuard.BindingID)
	assert.Equal(t, "github", h.store.lastImportGuard.Provider)
	assert.Equal(t, h.now, h.store.lastImportGuard.StartedAt)
	require.Len(t, h.sinkCalls, 1)
	assert.NotEmpty(t, h.sinkCalls[0].events)
	assert.Equal(t, h.project.ID, h.sinkCalls[0].projectID)
}

func TestRunnerUsesBindingSessionFetcherForOneRun(t *testing.T) {
	h := newRunnerHarness(t)
	issueTime := h.now.Add(-time.Hour)
	session := &fakeRunnerFetcher{
		repo: Repository{NodeID: h.binding.RemoteID, ID: 101, FullName: h.binding.DisplayName},
		issues: []Issue{
			testIssue(101, 1, "session issue", issueTime),
		},
	}
	sessionFetcher := &fakeBindingSessionFetcher{session: session}
	h.runner.config.Fetcher = sessionFetcher

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Import.Created)
	assert.Equal(t, []Binding{{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}}, sessionFetcher.bindings)
	require.Len(t, session.issueCalls, 1)
	assert.Empty(t, sessionFetcher.issueCalls)
}

func TestRunnerOversizedInitialSyncSplitsImportsAndAdvancesCursorAfterAllChunks(t *testing.T) {
	h := newRunnerHarness(t, withInitialBatchSize(2))
	issueTime := h.now.Add(-time.Hour)
	h.fetcher.issues = []Issue{
		testIssue(101, 1, "first issue", issueTime),
		testIssue(102, 2, "second issue", issueTime),
		testIssue(103, 3, "third issue", issueTime),
		testIssue(104, 4, "fourth issue", issueTime),
		testIssue(105, 5, "fifth issue", issueTime),
	}

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, 5, result.Import.Created)
	assert.Equal(t, []int{2, 2, 1}, h.store.importItemCounts)
	require.Len(t, h.sinkCalls, 3)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
}

func TestRunnerPartialMultiBatchFailureDeliversEarlierEventsAndLeavesCursorUnchanged(t *testing.T) {
	h := newRunnerHarness(t, withInitialBatchSize(2))
	h.store.failImportCall = 2
	h.store.importErr = errors.New("import failed")
	issueTime := h.now.Add(-time.Hour)
	h.fetcher.issues = []Issue{
		testIssue(101, 1, "first issue", issueTime),
		testIssue(102, 2, "second issue", issueTime),
		testIssue(103, 3, "third issue", issueTime),
	}

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.ErrorContains(t, err, "import failed")

	assert.Equal(t, []int{2, 1}, h.store.importItemCounts)
	require.Len(t, h.sinkCalls, 1)
	assert.NotEmpty(t, h.sinkCalls[0].events)
	assertCursorNil(h.ctx, t, h.db, h.binding.ID)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastErrorAt)
	assert.Contains(t, status.LastError, "import failed")
}

func TestRunnerParentDataFailureRecordsErrorAndLeavesExistingState(t *testing.T) {
	h := newRunnerHarness(t)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = []Issue{testIssue(101, 1, "first issue", h.now.Add(-time.Hour))}
	h.fetcher.parentMapErr = errors.New("parent lookup unavailable")

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.ErrorContains(t, err, "parent lookup unavailable")

	assert.Empty(t, h.store.importItemCounts)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, lastCursor)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastErrorAt)
	assert.Contains(t, status.LastError, "parent lookup unavailable")
}

func TestRunnerInitialSyncReconcilesParentLinksAfterAllChunksImport(t *testing.T) {
	h := newRunnerHarness(t, withInitialBatchSize(1))
	issueTime := h.now.Add(-time.Hour)
	h.fetcher.issues = []Issue{
		testIssue(101, 1, "child issue", issueTime),
		testIssue(102, 2, "parent issue", issueTime),
	}
	h.fetcher.parentMap = map[int]int64{1: 102}

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, []int{1, 1}, h.store.importItemCounts)
	assert.Equal(t, 2, result.Import.Created)
	assert.Equal(t, 1, result.Import.Links)
	childMapping, err := h.db.ImportMappingBySource(h.ctx, h.project.ID, h.binding.SourceKey, "issue", "issue-id:101")
	require.NoError(t, err)
	require.NotNil(t, childMapping.IssueID)
	parentMapping, err := h.db.ImportMappingBySource(h.ctx, h.project.ID, h.binding.SourceKey, "issue", "issue-id:102")
	require.NoError(t, err)
	require.NotNil(t, parentMapping.IssueID)
	parents, err := h.db.ParentNumbersByIssues(h.ctx, []int64{*childMapping.IssueID})
	require.NoError(t, err)
	assert.Equal(t, *parentMapping.IssueID, parents[*childMapping.IssueID])
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
}

func TestRunnerFeatureUnsupportedParentDataPreservesChangedIssueParent(t *testing.T) {
	h := newRunnerHarness(t)
	initialTime := h.now.Add(-2 * time.Hour)
	seedSourceParentLink(t, h, initialTime)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = []Issue{testIssue(101, 1, "changed child", h.now.Add(-time.Hour))}
	h.fetcher.parentData = ParentData{Unsupported: true}
	h.fetcher.parentDataSet = true

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assertSourceParent(t, h, "issue-id:101", "issue-id:102")
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
}

func TestRunnerBackfillsParentLinksWithOneFullImport(t *testing.T) {
	h := newRunnerHarness(t)
	initialTime := h.now.Add(-2 * time.Hour)
	seedSourceParentLink(t, h, initialTime)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = []Issue{
		testIssue(101, 1, "changed child", h.now.Add(-time.Hour)),
		testIssue(102, 2, "parent", h.now.Add(-time.Hour)),
	}
	h.fetcher.parentData = ParentData{
		ParentByChild:   map[int]int64{1: 102},
		ScannedChildren: map[int]struct{}{1: {}, 2: {}},
		Authoritative:   true,
	}
	h.fetcher.parentDataSet = true

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	require.Len(t, h.fetcher.issueCalls, 1)
	assert.Nil(t, h.fetcher.issueCalls[0].since)
	refreshed, err := h.db.IssueSyncBindingByID(h.ctx, h.binding.ID)
	require.NoError(t, err)
	refreshedConfig, err := DecodeConfig(refreshed.Config)
	require.NoError(t, err)
	assert.Equal(t, currentParentLinksVersion, refreshedConfig.ParentLinksVersion)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)

	firstRunAt := h.now
	h.advance(5 * time.Minute)
	h.fetcher.issueCalls = nil
	h.fetcher.issues = nil

	_, err = h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	require.Len(t, h.fetcher.issueCalls, 1)
	require.NotNil(t, h.fetcher.issueCalls[0].since)
	assert.Equal(t, firstRunAt.Add(-2*time.Minute), *h.fetcher.issueCalls[0].since)
}

func TestRunnerBackfillReconcilesParentLinksForUnchangedExistingIssues(t *testing.T) {
	h := newRunnerHarness(t)
	initialTime := h.now.Add(-2 * time.Hour)
	seedBatch := BuildImportBatchWithConfig(h.binding.SourceKey, Config{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	}, []Issue{
		testIssue(101, 1, "unchanged child", initialTime),
		testIssue(102, 2, "unchanged parent", initialTime),
	}, nil, ParentData{}, h.now)
	seedBatch.ProjectID = h.project.ID
	_, _, err := h.db.ImportBatch(h.ctx, seedBatch)
	require.NoError(t, err)
	childMapping, err := h.db.ImportMappingBySource(h.ctx, h.project.ID, h.binding.SourceKey, "issue", "issue-id:101")
	require.NoError(t, err)
	require.NotNil(t, childMapping.IssueID)
	parents, err := h.db.ParentNumbersByIssues(h.ctx, []int64{*childMapping.IssueID})
	require.NoError(t, err)
	assert.NotContains(t, parents, *childMapping.IssueID)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = []Issue{
		testIssue(101, 1, "unchanged child", initialTime),
		testIssue(102, 2, "unchanged parent", initialTime),
	}
	h.fetcher.parentData = ParentData{
		ParentByChild:   map[int]int64{1: 102},
		ScannedChildren: map[int]struct{}{1: {}, 2: {}},
		Authoritative:   true,
	}
	h.fetcher.parentDataSet = true

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, 2, result.Import.Unchanged)
	assert.Equal(t, 1, result.Import.Links)
	assertSourceParent(t, h, "issue-id:101", "issue-id:102")
	refreshed, err := h.db.IssueSyncBindingByID(h.ctx, h.binding.ID)
	require.NoError(t, err)
	refreshedConfig, err := DecodeConfig(refreshed.Config)
	require.NoError(t, err)
	assert.Equal(t, currentParentLinksVersion, refreshedConfig.ParentLinksVersion)
}

func TestRunnerReconcilesScannedParentForIssueAbsentFromIncrementalFetch(t *testing.T) {
	h := newRunnerHarness(t)
	initialTime := h.now.Add(-2 * time.Hour)
	seedBatch := BuildImportBatchWithConfig(h.binding.SourceKey, Config{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	}, []Issue{
		testIssue(101, 1, "child", initialTime),
		testIssue(102, 2, "old parent", initialTime),
		testIssue(103, 3, "new parent", initialTime),
	}, nil, ParentData{
		ParentByChild:   map[int]int64{1: 102},
		ScannedChildren: map[int]struct{}{1: {}, 2: {}, 3: {}},
		ChildIDByNumber: map[int]int64{1: 101, 2: 102, 3: 103},
		Authoritative:   true,
	}, h.now)
	seedBatch.ProjectID = h.project.ID
	_, _, err := h.db.ImportBatch(h.ctx, seedBatch)
	require.NoError(t, err)
	assertSourceParent(t, h, "issue-id:101", "issue-id:102")
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = nil
	h.fetcher.parentData = ParentData{
		ParentByChild:   map[int]int64{1: 103},
		ScannedChildren: map[int]struct{}{1: {}, 2: {}, 3: {}},
		ChildIDByNumber: map[int]int64{1: 101, 2: 102, 3: 103},
		Authoritative:   true,
	}
	h.fetcher.parentDataSet = true

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, 3, result.Import.Unchanged)
	assert.Equal(t, 1, result.Import.Links)
	assertSourceParent(t, h, "issue-id:101", "issue-id:103")
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
}

func TestRunnerRemovesScannedParentForIssueAbsentFromIncrementalFetch(t *testing.T) {
	h := newRunnerHarness(t)
	initialTime := h.now.Add(-2 * time.Hour)
	seedSourceParentLink(t, h, initialTime)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = nil
	h.fetcher.parentData = ParentData{
		ScannedChildren: map[int]struct{}{1: {}, 2: {}},
		ChildIDByNumber: map[int]int64{1: 101, 2: 102},
		Authoritative:   true,
	}
	h.fetcher.parentDataSet = true

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, 2, result.Import.Unchanged)
	assert.Equal(t, 0, result.Import.Links)
	assertNoParent(t, h, "issue-id:101")
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
}

func TestRunnerBackfillConfigPersistFailureRecordsErrorAndClearsInFlight(t *testing.T) {
	h := newRunnerHarness(t)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = []Issue{
		testIssue(101, 1, "child", h.now.Add(-time.Hour)),
		testIssue(102, 2, "parent", h.now.Add(-time.Hour)),
	}
	h.fetcher.parentData = ParentData{
		ParentByChild:   map[int]int64{1: 102},
		ScannedChildren: map[int]struct{}{1: {}, 2: {}},
		Authoritative:   true,
	}
	h.fetcher.parentDataSet = true
	h.store.refreshErr = errors.New("config persist unavailable")

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.ErrorContains(t, err, "config persist unavailable")

	assert.Equal(t, 2, result.Import.Created)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, lastCursor)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastErrorAt)
	assert.Contains(t, status.LastError, "config persist unavailable")
}

func TestRunnerMissingParentTargetSkipsLinkAndPreservesExistingParent(t *testing.T) {
	h := newRunnerHarness(t)
	initialTime := h.now.Add(-2 * time.Hour)
	seedSourceParentLink(t, h, initialTime)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = []Issue{testIssue(101, 1, "changed child", h.now.Add(-time.Hour))}
	h.fetcher.parentData = ParentData{
		ParentByChild:   map[int]int64{1: 999},
		ScannedChildren: map[int]struct{}{1: {}},
		Authoritative:   true,
	}
	h.fetcher.parentDataSet = true

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assertSourceParent(t, h, "issue-id:101", "issue-id:102")
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
}

func TestRunnerParentTargetLookupErrorRecordsFailureAndSkipsImport(t *testing.T) {
	h := newRunnerHarness(t)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = []Issue{testIssue(101, 1, "changed child", h.now.Add(-time.Hour))}
	h.fetcher.parentData = ParentData{
		ParentByChild:   map[int]int64{1: 999},
		ScannedChildren: map[int]struct{}{1: {}},
		Authoritative:   true,
	}
	h.fetcher.parentDataSet = true
	h.store.importMappingErr = errors.New("mapping lookup unavailable")

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.ErrorContains(t, err, "mapping lookup unavailable")

	assert.Empty(t, h.store.importItemCounts)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, lastCursor)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastErrorAt)
	assert.Contains(t, status.LastError, "mapping lookup unavailable")
}

func TestRunnerChildAbsentFromParentScanPreservesChangedIssueParent(t *testing.T) {
	h := newRunnerHarness(t)
	initialTime := h.now.Add(-2 * time.Hour)
	seedSourceParentLink(t, h, initialTime)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = []Issue{testIssue(101, 1, "changed child", h.now.Add(-time.Hour))}
	h.fetcher.parentData = ParentData{
		ScannedChildren: map[int]struct{}{2: {}},
		Authoritative:   true,
	}
	h.fetcher.parentDataSet = true

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assertSourceParent(t, h, "issue-id:101", "issue-id:102")
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
}

func TestRunnerNegativeInitialBatchSizeDefaults(t *testing.T) {
	h := newRunnerHarness(t, withInitialBatchSize(-1))
	issueTime := h.now.Add(-time.Hour)
	h.fetcher.issues = []Issue{
		testIssue(101, 1, "first issue", issueTime),
		testIssue(102, 2, "second issue", issueTime),
		testIssue(103, 3, "third issue", issueTime),
	}

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, 3, result.Import.Created)
	assert.Equal(t, []int{3}, h.store.importItemCounts)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
}

func TestRunnerEventSinkErrorIsNonFatal(t *testing.T) {
	h := newRunnerHarness(t)
	h.sinkErr = errors.New("sink unavailable")
	h.fetcher.issues = []Issue{testIssue(101, 1, "first issue", h.now.Add(-time.Hour))}

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Import.Created)
	require.Len(t, h.sinkCalls, 1)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.LastErrorAt)
	assert.Empty(t, status.LastError)
}

func TestRunnerSuccessCleanupIgnoresCallerCancellation(t *testing.T) {
	h := newRunnerHarness(t)
	ctx, cancel := context.WithCancel(h.ctx)
	h.runner.config.EventSink = func(context.Context, int64, []db.Event) error {
		cancel()
		return nil
	}
	h.fetcher.issues = []Issue{testIssue(101, 1, "first issue", h.now.Add(-time.Hour))}

	result, err := h.runner.RunOnce(ctx, h.binding.ID)

	require.NoError(t, err)
	assert.NoError(t, h.store.successCtxErr)
	assert.Equal(t, 1, result.Import.Created)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
}

func TestRunnerIncrementalUsesCursorMinusOverlap(t *testing.T) {
	h := newRunnerHarness(t)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issues = []Issue{testIssue(101, 1, "first issue", h.now.Add(-time.Hour))}

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	require.Len(t, h.fetcher.issueCalls, 1)
	require.NotNil(t, h.fetcher.issueCalls[0].since)
	assert.Equal(t, lastCursor.Add(-2*time.Minute), *h.fetcher.issueCalls[0].since)
}

func TestRunnerLegacyTitlePrefixConfigReconcilesExistingImportedTitles(t *testing.T) {
	h := newRunnerHarness(t)
	issueTime := h.now.Add(-time.Hour)
	exactTitle := testIssue(101, 1, "legacy title", issueTime)
	noTitlePrefix := false
	exactBatch := BuildImportBatchWithConfig(h.binding.SourceKey, Config{
		Host:        "github.com",
		Owner:       "example-owner",
		Repo:        "example-repo",
		RepoID:      101,
		TitlePrefix: &noTitlePrefix,
	}, []Issue{exactTitle}, nil, ParentData{}, h.now)
	exactBatch.ProjectID = h.project.ID
	_, _, err := h.db.ImportBatch(h.ctx, exactBatch)
	require.NoError(t, err)
	mapping, err := h.db.ImportMappingBySource(h.ctx, h.project.ID, h.binding.SourceKey, "issue", "issue-id:101")
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
	assert.Equal(t, "legacy title", issueTitleByID(h.ctx, t, h.db, *mapping.IssueID))
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	legacyConfig := json.RawMessage(`{"host":"github.com","owner":"example-owner","repo":"example-repo","repo_id":101}`)
	_, err = h.db.ExecContext(h.ctx, `UPDATE issue_sync_bindings SET config_json = ? WHERE id = ?`, string(legacyConfig), h.binding.ID)
	require.NoError(t, err)
	h.fetcher.issues = []Issue{exactTitle}

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	require.Len(t, h.fetcher.issueCalls, 1)
	assert.Nil(t, h.fetcher.issueCalls[0].since)
	assert.Equal(t, 1, result.Import.Updated)
	assert.Equal(t, "[GitHub #1] legacy title", issueTitleByID(h.ctx, t, h.db, *mapping.IssueID))
	refreshed, err := h.db.IssueSyncBindingByID(h.ctx, h.binding.ID)
	require.NoError(t, err)
	refreshedConfig, err := DecodeConfig(refreshed.Config)
	require.NoError(t, err)
	require.NotNil(t, refreshedConfig.TitlePrefix)
	assert.True(t, *refreshedConfig.TitlePrefix)
}

func TestRunnerFetchesCommentsForReturnedNonPullRequestIssuesInOverlapWindow(t *testing.T) {
	h := newRunnerHarness(t)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	overlapIssue := testIssue(101, 1, "overlap issue", lastCursor.Add(-time.Minute))
	overlapIssue.Comments = 1
	equalCursorIssue := testIssue(102, 2, "equal cursor issue", lastCursor)
	equalCursorIssue.Comments = 1
	pullRequest := testIssue(103, 3, "pull request", lastCursor.Add(-time.Minute))
	pullRequest.Comments = 1
	pullRequest.PullRequest = &PullRequest{}
	h.fetcher.issues = []Issue{overlapIssue, equalCursorIssue, pullRequest}

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, []int{1, 2}, h.fetcher.commentCalls)
}

func TestRunnerSkipsCommentFetchWhenGitHubIssueReportsZeroComments(t *testing.T) {
	h := newRunnerHarness(t)
	withComments := testIssue(101, 1, "with comments", h.now.Add(-time.Hour))
	withComments.Comments = 2
	withoutComments := testIssue(102, 2, "without comments", h.now.Add(-time.Hour))
	h.fetcher.issues = []Issue{withComments, withoutComments}
	h.fetcher.comments = map[int][]Comment{
		1: {{
			ID:        1001,
			NodeID:    "C_with_1",
			Body:      "comment body",
			User:      &User{Login: "commenter"},
			CreatedAt: ptrTime(h.now.Add(-time.Minute)),
		}},
	}

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, []int{1}, h.fetcher.commentCalls)
	assert.Equal(t, 2, result.Import.Created)
	assert.Equal(t, 1, result.Import.Comments)
}

func TestRunnerRepositoryMetadataRefreshUpdatesMutableFieldsWhenNodeMatches(t *testing.T) {
	h := newRunnerHarness(t)
	h.fetcher.repo = Repository{NodeID: h.binding.RemoteID, ID: 2020, FullName: "example-owner/renamed-repo"}
	h.fetcher.issues = []Issue{testIssue(101, 1, "first issue", h.now.Add(-time.Hour))}

	result, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)

	assert.Equal(t, "example-owner/renamed-repo", result.Binding.DisplayName)
	resultConfig, err := DecodeConfig(result.Binding.Config)
	require.NoError(t, err)
	assert.Equal(t, "renamed-repo", resultConfig.Repo)
	assert.Equal(t, int64(2020), resultConfig.RepoID)
	refreshed, err := h.db.IssueSyncBindingByID(h.ctx, h.binding.ID)
	require.NoError(t, err)
	assert.Equal(t, "example-owner/renamed-repo", refreshed.DisplayName)
	refreshedConfig, err := DecodeConfig(refreshed.Config)
	require.NoError(t, err)
	assert.Equal(t, "renamed-repo", refreshedConfig.Repo)
	assert.Equal(t, int64(2020), refreshedConfig.RepoID)
}

func TestRunnerRepositoryNodeMismatchRecordsErrorAndImportsNothing(t *testing.T) {
	h := newRunnerHarness(t)
	h.fetcher.repo = Repository{NodeID: "R_other_repo", ID: 2020, FullName: "example-owner/example-repo"}
	h.fetcher.issues = []Issue{testIssue(101, 1, "first issue", h.now.Add(-time.Hour))}

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.ErrorContains(t, err, "repository node")

	assert.Empty(t, h.fetcher.issueCalls)
	assert.Empty(t, h.store.importItemCounts)
	assertCursorNil(h.ctx, t, h.db, h.binding.ID)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastErrorAt)
	assert.Contains(t, status.LastError, "repository node")
}

func TestRunnerFetchFailureClearsInFlightAndLeavesCursorUnchanged(t *testing.T) {
	h := newRunnerHarness(t)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.fetcher.issuesErr = errors.New("github unavailable")

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.ErrorContains(t, err, "github unavailable")

	assertCursorAt(h.ctx, t, h.db, h.binding.ID, lastCursor)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastErrorAt)
	assert.Contains(t, status.LastError, "github unavailable")
}

func TestRunnerErrorCleanupIgnoresCallerCancellation(t *testing.T) {
	h := newRunnerHarness(t)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	ctx, cancel := context.WithCancel(h.ctx)
	h.fetcher.beforeIssuesReturn = cancel
	h.fetcher.issuesErr = errors.New("github unavailable")

	_, err := h.runner.RunOnce(ctx, h.binding.ID)

	require.ErrorContains(t, err, "github unavailable")
	assert.NoError(t, h.store.errorCtxErr)
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, lastCursor)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastErrorAt)
	assert.Contains(t, status.LastError, "github unavailable")
}

func TestRunnerImportFailureClearsInFlightAndLeavesCursorUnchanged(t *testing.T) {
	h := newRunnerHarness(t)
	lastCursor := h.now.Add(-10 * time.Minute)
	recordSuccessfulCursor(h.ctx, t, h.db, h.binding.ID, lastCursor)
	h.store.failImportCall = 1
	h.store.importErr = errors.New("import unavailable")
	h.fetcher.issues = []Issue{testIssue(101, 1, "first issue", h.now.Add(-time.Hour))}

	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.ErrorContains(t, err, "import unavailable")

	assertCursorAt(h.ctx, t, h.db, h.binding.ID, lastCursor)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastErrorAt)
	assert.Contains(t, status.LastError, "import unavailable")
}

func TestRunnerNonStaleInFlightBindingReturnsAlreadyRunning(t *testing.T) {
	h := newRunnerHarness(t)
	_, ok, err := h.db.ClaimIssueSyncBinding(h.ctx, h.binding.ID, "github", h.now.Add(-time.Minute), h.now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)

	_, err = h.runner.RunOnce(h.ctx, h.binding.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)

	assert.Empty(t, h.fetcher.repoCallsSnapshot())
}

func TestRunnerNegativeStaleLockTTLDefaults(t *testing.T) {
	h := newRunnerHarness(t, withStaleLockTTL(-time.Hour))
	_, ok, err := h.db.ClaimIssueSyncBinding(h.ctx, h.binding.ID, "github", h.now.Add(-time.Minute), h.now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)

	_, err = h.runner.RunOnce(h.ctx, h.binding.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)

	assert.Empty(t, h.fetcher.repoCallsSnapshot())
}

func TestRunnerRunSelectsDueBindingsAndRunsSerially(t *testing.T) {
	h := newRunnerHarness(t)
	secondProject, err := h.db.CreateProject(h.ctx, "hub-project")
	require.NoError(t, err)
	secondBinding, err := h.db.UpsertIssueSyncBinding(h.ctx, db.UpsertIssueSyncBindingParams{
		ProjectID:       secondProject.ID,
		Provider:        "github",
		SourceKey:       "github:R_second_repo",
		RemoteID:        "R_second_repo",
		DisplayName:     "example-owner/second-repo",
		Config:          mustTestGitHubConfig(t, "github.com", "example-owner", "second-repo", 202),
		IntervalSeconds: 300,
	})
	require.NoError(t, err)
	h.fetcher.repos = map[string]Repository{
		"example-repo": {NodeID: h.binding.RemoteID, ID: 101, FullName: "example-owner/example-repo"},
		"second-repo":  {NodeID: secondBinding.RemoteID, ID: 202, FullName: "example-owner/second-repo"},
	}
	h.fetcher.issuesByRepo = map[string][]Issue{
		"example-repo": {testIssue(101, 1, "first issue", h.now.Add(-time.Hour))},
		"second-repo":  {testIssue(201, 1, "second issue", h.now.Add(-time.Hour))},
	}
	h.fetcher.blockRepository = make(chan struct{})
	h.fetcher.releaseRepository = make(chan struct{})

	runDone := make(chan error, 1)
	go func() {
		runDone <- h.runner.Run(h.ctx)
	}()

	select {
	case <-h.fetcher.blockRepository:
	case err := <-runDone:
		t.Fatalf("Run returned before first binding blocked: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first repository fetch")
	}
	assert.Len(t, h.fetcher.repoCallsSnapshot(), 1)
	close(h.fetcher.releaseRepository)

	require.NoError(t, <-runDone)
	assert.Equal(t, []string{"example-repo", "second-repo"}, h.fetcher.repoCallsSnapshot())
	assertCursorAt(h.ctx, t, h.db, h.binding.ID, h.now)
	assertCursorAt(h.ctx, t, h.db, secondBinding.ID, h.now)
}

func TestRunnerRunAttemptsLaterDueBindingsAfterBindingFailure(t *testing.T) {
	h := newRunnerHarness(t)
	secondBinding := h.mustCreateBinding("hub-project", "R_second_repo", "second-repo", 202)
	h.fetcher.repos = map[string]Repository{
		"second-repo": {NodeID: secondBinding.RemoteID, ID: 202, FullName: "example-owner/second-repo"},
	}
	h.fetcher.repoErrs = map[string]error{
		"example-repo": errors.New("github unavailable"),
	}
	h.fetcher.issuesByRepo = map[string][]Issue{
		"second-repo": {testIssue(201, 1, "second issue", h.now.Add(-time.Hour))},
	}

	err := h.runner.Run(h.ctx)
	require.ErrorContains(t, err, "github unavailable")

	assert.Equal(t, []string{"example-repo", "second-repo"}, h.fetcher.repoCallsSnapshot())
	assertCursorNil(h.ctx, t, h.db, h.binding.ID)
	assertCursorAt(h.ctx, t, h.db, secondBinding.ID, h.now)
	status, err := h.db.IssueSyncStatusByProject(h.ctx, h.project.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	assert.Contains(t, status.LastError, "github unavailable")
}

func TestRunnerIntervalModeLogsBindingFailuresAndKeepsRunning(t *testing.T) {
	h := newRunnerHarness(t, withInterval(time.Hour))
	secondBinding := h.mustCreateBinding("hub-project", "R_second_repo", "second-repo", 202)
	h.fetcher.repos = map[string]Repository{
		"second-repo": {NodeID: secondBinding.RemoteID, ID: 202, FullName: "example-owner/second-repo"},
	}
	h.fetcher.repoErrs = map[string]error{
		"example-repo": errors.New("github unavailable"),
	}
	h.fetcher.issuesByRepo = map[string][]Issue{
		"second-repo": {testIssue(201, 1, "second issue", h.now.Add(-time.Hour))},
	}

	ctx, cancel := context.WithCancel(h.ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- h.runner.Run(ctx)
	}()

	require.Eventually(t, func() bool {
		got, err := h.db.IssueSyncBindingByID(h.ctx, secondBinding.ID)
		return err == nil && got.LastCursorAt != nil && got.LastCursorAt.Equal(h.now)
	}, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-runDone, context.Canceled)
	assert.Equal(t, []string{"example-repo", "second-repo"}, h.fetcher.repoCallsSnapshot())
}

func TestGitHubSyncRunnerRunWakesBeforeInterval(t *testing.T) {
	wake := make(chan struct{}, 1)
	h := newRunnerHarness(t, withInterval(time.Hour), withWake(wake))
	h.fetcher.issues = []Issue{testIssue(101, 1, "first issue", h.now.Add(-time.Hour))}

	ctx, cancel := context.WithCancel(h.ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- h.runner.Run(ctx)
	}()

	require.Eventually(t, func() bool {
		got, err := h.db.IssueSyncBindingByID(h.ctx, h.binding.ID)
		return err == nil && got.LastCursorAt != nil && h.fetcher.repoCallCount() == 1
	}, time.Second, time.Millisecond)

	h.advance(6 * time.Minute)
	wake <- struct{}{}

	require.Eventually(t, func() bool {
		return h.fetcher.repoCallCount() == 2
	}, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-runDone, context.Canceled)
}

func TestGitHubSyncRunnerRunDoesNotOverlapWakeWhileBindingIsInFlight(t *testing.T) {
	wake := make(chan struct{}, 5)
	h := newRunnerHarness(t, withInterval(time.Hour), withWake(wake))
	h.fetcher.issues = []Issue{testIssue(101, 1, "first issue", h.now.Add(-time.Hour))}
	h.fetcher.blockRepository = make(chan struct{})
	h.fetcher.releaseRepository = make(chan struct{})

	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- h.runner.Run(ctx)
	}()

	select {
	case <-h.fetcher.blockRepository:
	case err := <-runDone:
		t.Fatalf("Run returned before repository fetch blocked: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for repository fetch")
	}

	for i := 0; i < cap(wake); i++ {
		wake <- struct{}{}
	}
	require.Never(t, func() bool {
		return h.fetcher.repoCallCount() > 1
	}, 50*time.Millisecond, time.Millisecond)

	close(h.fetcher.releaseRepository)
	require.Eventually(t, func() bool {
		got, err := h.db.IssueSyncBindingByID(h.ctx, h.binding.ID)
		return err == nil && got.LastCursorAt != nil
	}, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-runDone, context.Canceled)
	assert.Equal(t, []string{"example-repo"}, h.fetcher.repoCallsSnapshot())
}

func TestRunnerRunRequiresStore(t *testing.T) {
	runner := NewRunner(RunnerConfig{})
	var err error
	require.NotPanics(t, func() {
		err = runner.Run(context.Background())
	})
	require.ErrorContains(t, err, "github sync runner requires store")
}

type runnerHarness struct {
	t         *testing.T
	ctx       context.Context
	db        *sqlitestore.Store
	store     *spyStorage
	project   db.Project
	binding   db.IssueSyncBinding
	fetcher   *fakeRunnerFetcher
	now       time.Time
	runner    *Runner
	sinkErr   error
	sinkCalls []sinkCall
}

type runnerOption func(*RunnerConfig)

func withInitialBatchSize(size int) runnerOption {
	return func(config *RunnerConfig) {
		config.InitialBatchSize = size
	}
}

func withStaleLockTTL(ttl time.Duration) runnerOption {
	return func(config *RunnerConfig) {
		config.StaleLockTTL = ttl
	}
}

func withInterval(interval time.Duration) runnerOption {
	return func(config *RunnerConfig) {
		config.Interval = interval
	}
}

func withWake(wake <-chan struct{}) runnerOption {
	return func(config *RunnerConfig) {
		config.Wake = wake
	}
}

func newRunnerHarness(t *testing.T, opts ...runnerOption) *runnerHarness {
	t.Helper()
	ctx := context.Background()
	t.Setenv("KATA_HOME", t.TempDir())
	storePath := filepath.Join(t.TempDir(), "kata.db")
	sqliteStore, err := sqlitestore.Open(ctx, storePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqliteStore.Close() })
	project, err := sqliteStore.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	binding, err := sqliteStore.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
		ProjectID:       project.ID,
		Provider:        "github",
		SourceKey:       "github:R_example_repo",
		RemoteID:        "R_example_repo",
		DisplayName:     "example-owner/example-repo",
		Config:          mustTestGitHubConfig(t, "github.com", "example-owner", "example-repo", 101),
		IntervalSeconds: 300,
	})
	require.NoError(t, err)
	fetcher := &fakeRunnerFetcher{
		repo: Repository{NodeID: binding.RemoteID, ID: 101, FullName: binding.DisplayName},
	}
	h := &runnerHarness{
		t:       t,
		ctx:     ctx,
		db:      sqliteStore,
		store:   &spyStorage{Storage: sqliteStore},
		project: project,
		binding: binding,
		fetcher: fetcher,
		now:     time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC),
	}
	config := &RunnerConfig{
		Store:        h.store,
		Fetcher:      fetcher,
		Clock:        func() time.Time { return h.now },
		EventSink:    h.recordSink,
		Logger:       slog.New(slog.NewTextHandler(testDiscardWriter{}, nil)),
		Interval:     0,
		StaleLockTTL: time.Hour,
	}
	for _, opt := range opts {
		opt(config)
	}
	h.runner = NewRunner(*config)
	return h
}

func (h *runnerHarness) advance(d time.Duration) {
	h.now = h.now.Add(d)
}

func (h *runnerHarness) mustCreateBinding(projectName, repoNodeID, repo string, repoID int64) db.IssueSyncBinding {
	h.t.Helper()
	project, err := h.db.CreateProject(h.ctx, projectName)
	require.NoError(h.t, err)
	binding, err := h.db.UpsertIssueSyncBinding(h.ctx, db.UpsertIssueSyncBindingParams{
		ProjectID:       project.ID,
		Provider:        "github",
		SourceKey:       "github:" + repoNodeID,
		RemoteID:        repoNodeID,
		DisplayName:     "example-owner/" + repo,
		Config:          mustTestGitHubConfig(h.t, "github.com", "example-owner", repo, repoID),
		IntervalSeconds: 300,
	})
	require.NoError(h.t, err)
	return binding
}

func mustTestGitHubConfig(t testing.TB, host, owner, repo string, repoID int64) []byte {
	t.Helper()
	config, err := EncodeConfig(Config{
		Host:   host,
		Owner:  owner,
		Repo:   repo,
		RepoID: repoID,
	})
	require.NoError(t, err)
	return config
}

func (h *runnerHarness) recordSink(_ context.Context, projectID int64, events []db.Event) error {
	h.sinkCalls = append(h.sinkCalls, sinkCall{
		projectID: projectID,
		events:    append([]db.Event(nil), events...),
	})
	return h.sinkErr
}

type sinkCall struct {
	projectID int64
	events    []db.Event
}

type spyStorage struct {
	db.Storage
	failImportCall   int
	importErr        error
	importMappingErr error
	importCalls      int
	importItemCounts []int
	lastImportGuard  *db.IssueSyncImportGuard
	successCtxErr    error
	errorCtxErr      error
	refreshErr       error
}

func (s *spyStorage) ImportBatch(ctx context.Context, p db.ImportBatchParams) (db.ImportBatchResult, []db.Event, error) {
	s.importCalls++
	s.importItemCounts = append(s.importItemCounts, len(p.Items))
	s.lastImportGuard = p.IssueSyncGuard
	if s.failImportCall == s.importCalls {
		return db.ImportBatchResult{}, nil, s.importErr
	}
	return s.Storage.ImportBatch(ctx, p)
}

func (s *spyStorage) ImportMappingBySource(ctx context.Context, projectID int64, source, objectType, externalID string) (db.ImportMapping, error) {
	if s.importMappingErr != nil {
		return db.ImportMapping{}, s.importMappingErr
	}
	return s.Storage.ImportMappingBySource(ctx, projectID, source, objectType, externalID)
}

func (s *spyStorage) RecordIssueSyncSuccess(ctx context.Context, p db.IssueSyncSuccessParams) (db.IssueSyncStatus, error) {
	s.successCtxErr = ctx.Err()
	return s.Storage.RecordIssueSyncSuccess(ctx, p)
}

func (s *spyStorage) RecordIssueSyncError(ctx context.Context, p db.IssueSyncErrorParams) (db.IssueSyncStatus, error) {
	s.errorCtxErr = ctx.Err()
	return s.Storage.RecordIssueSyncError(ctx, p)
}

func (s *spyStorage) RefreshIssueSyncBinding(ctx context.Context, p db.IssueSyncBindingUpdateParams) (db.IssueSyncBinding, error) {
	if s.refreshErr != nil {
		return db.IssueSyncBinding{}, s.refreshErr
	}
	return s.Storage.RefreshIssueSyncBinding(ctx, p)
}

type fakeRunnerFetcher struct {
	mu                 sync.Mutex
	repo               Repository
	repos              map[string]Repository
	repoErr            error
	repoErrs           map[string]error
	issues             []Issue
	issuesByRepo       map[string][]Issue
	issuesErr          error
	beforeIssuesReturn func()
	comments           map[int][]Comment
	commentsErr        error
	parentData         ParentData
	parentDataSet      bool
	parentMap          map[int]int64
	parentMapErr       error
	repoCalls          []string
	issueCalls         []fakeIssueCall
	commentCalls       []int
	blockRepository    chan struct{}
	releaseRepository  chan struct{}
	blockOnce          sync.Once
}

type fakeBindingSessionFetcher struct {
	fakeRunnerFetcher
	session  Fetcher
	bindings []Binding
}

func (f *fakeBindingSessionFetcher) ForBinding(_ context.Context, binding Binding) (Fetcher, error) {
	f.bindings = append(f.bindings, binding)
	return f.session, nil
}

func (f *fakeRunnerFetcher) Repository(_ context.Context, _, _ string, repo string) (Repository, error) {
	f.mu.Lock()
	f.repoCalls = append(f.repoCalls, repo)
	f.mu.Unlock()
	if f.blockRepository != nil {
		f.blockOnce.Do(func() {
			f.blockRepository <- struct{}{}
			<-f.releaseRepository
		})
	}
	if f.repoErr != nil {
		return Repository{}, f.repoErr
	}
	if f.repoErrs != nil && f.repoErrs[repo] != nil {
		return Repository{}, f.repoErrs[repo]
	}
	if f.repos != nil {
		return f.repos[repo], nil
	}
	return f.repo, nil
}

func (f *fakeRunnerFetcher) Issues(_ context.Context, binding Binding, since *time.Time) ([]Issue, error) {
	var sinceCopy *time.Time
	if since != nil {
		v := *since
		sinceCopy = &v
	}
	f.mu.Lock()
	f.issueCalls = append(f.issueCalls, fakeIssueCall{binding: binding, since: sinceCopy})
	f.mu.Unlock()
	if f.beforeIssuesReturn != nil {
		f.beforeIssuesReturn()
	}
	if f.issuesErr != nil {
		return nil, f.issuesErr
	}
	if f.issuesByRepo != nil {
		return append([]Issue(nil), f.issuesByRepo[binding.Repo]...), nil
	}
	return append([]Issue(nil), f.issues...), nil
}

func (f *fakeRunnerFetcher) Comments(_ context.Context, _ Binding, issueNumber int) ([]Comment, error) {
	f.mu.Lock()
	f.commentCalls = append(f.commentCalls, issueNumber)
	f.mu.Unlock()
	if f.commentsErr != nil {
		return nil, f.commentsErr
	}
	return append([]Comment(nil), f.comments[issueNumber]...), nil
}

func (f *fakeRunnerFetcher) ParentData(_ context.Context, _ Binding) (ParentData, error) {
	if f.parentMapErr != nil {
		return ParentData{}, f.parentMapErr
	}
	if f.parentDataSet {
		return cloneParentData(f.parentData), nil
	}
	if f.parentMap == nil {
		return ParentData{}, nil
	}
	out := make(map[int]int64, len(f.parentMap))
	scanned := make(map[int]struct{}, len(f.parentMap))
	for k, v := range f.parentMap {
		out[k] = v
		scanned[k] = struct{}{}
	}
	return ParentData{ParentByChild: out, ScannedChildren: scanned, Authoritative: true}, nil
}

func cloneParentData(in ParentData) ParentData {
	out := in
	if in.ParentByChild != nil {
		out.ParentByChild = make(map[int]int64, len(in.ParentByChild))
		for k, v := range in.ParentByChild {
			out.ParentByChild[k] = v
		}
	}
	if in.ScannedChildren != nil {
		out.ScannedChildren = make(map[int]struct{}, len(in.ScannedChildren))
		for k, v := range in.ScannedChildren {
			out.ScannedChildren[k] = v
		}
	}
	if in.ChildIDByNumber != nil {
		out.ChildIDByNumber = make(map[int]int64, len(in.ChildIDByNumber))
		for k, v := range in.ChildIDByNumber {
			out.ChildIDByNumber[k] = v
		}
	}
	return out
}

func seedSourceParentLink(t *testing.T, h *runnerHarness, ts time.Time) {
	t.Helper()
	_, _, err := h.db.ImportBatch(h.ctx, db.ImportBatchParams{
		ProjectID: h.project.ID,
		Source:    h.binding.SourceKey,
		Actor:     actorGitHubSync,
		Items: []db.ImportItem{
			{
				ExternalID: "issue-id:101",
				Title:      "child",
				Body:       "body",
				Author:     "alice",
				Status:     "open",
				CreatedAt:  ts,
				UpdatedAt:  ts,
				Links:      []db.ImportLink{{Type: "parent", TargetExternalID: "issue-id:102"}},
			},
			{
				ExternalID: "issue-id:102",
				Title:      "parent",
				Body:       "body",
				Author:     "bob",
				Status:     "open",
				CreatedAt:  ts,
				UpdatedAt:  ts,
			},
		},
	})
	require.NoError(t, err)
	assertSourceParent(t, h, "issue-id:101", "issue-id:102")
}

func assertSourceParent(t *testing.T, h *runnerHarness, childExternalID, parentExternalID string) {
	t.Helper()
	childMapping, err := h.db.ImportMappingBySource(h.ctx, h.project.ID, h.binding.SourceKey, "issue", childExternalID)
	require.NoError(t, err)
	require.NotNil(t, childMapping.IssueID)
	parentMapping, err := h.db.ImportMappingBySource(h.ctx, h.project.ID, h.binding.SourceKey, "issue", parentExternalID)
	require.NoError(t, err)
	require.NotNil(t, parentMapping.IssueID)
	parents, err := h.db.ParentNumbersByIssues(h.ctx, []int64{*childMapping.IssueID})
	require.NoError(t, err)
	assert.Equal(t, *parentMapping.IssueID, parents[*childMapping.IssueID])
}

func assertNoParent(t *testing.T, h *runnerHarness, childExternalID string) {
	t.Helper()
	childMapping, err := h.db.ImportMappingBySource(h.ctx, h.project.ID, h.binding.SourceKey, "issue", childExternalID)
	require.NoError(t, err)
	require.NotNil(t, childMapping.IssueID)
	parents, err := h.db.ParentNumbersByIssues(h.ctx, []int64{*childMapping.IssueID})
	require.NoError(t, err)
	assert.NotContains(t, parents, *childMapping.IssueID)
}

func (f *fakeRunnerFetcher) repoCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.repoCalls)
}

func (f *fakeRunnerFetcher) repoCallsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.repoCalls...)
}

type fakeIssueCall struct {
	binding Binding
	since   *time.Time
}

func testIssue(id int64, number int, title string, ts time.Time) Issue {
	return Issue{
		ID:        id,
		NodeID:    "I_example",
		Number:    number,
		HTMLURL:   "https://github.com/example-owner/example-repo/issues/1",
		Title:     title,
		Body:      "body",
		State:     "open",
		User:      &User{Login: "author"},
		CreatedAt: ptrTime(ts.Add(-time.Minute)),
		UpdatedAt: ptrTime(ts),
	}
}

func recordSuccessfulCursor(ctx context.Context, t *testing.T, store db.Storage, bindingID int64, cursor time.Time) {
	t.Helper()
	started := cursor.Add(-time.Minute)
	_, ok, err := store.ClaimIssueSyncBinding(ctx, bindingID, "github", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = store.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{
		BindingID: bindingID,
		StartedAt: started,
		At:        cursor,
		CursorAt:  cursor,
	})
	require.NoError(t, err)
}

func assertCursorAt(ctx context.Context, t *testing.T, store db.Storage, bindingID int64, want time.Time) {
	t.Helper()
	got, err := store.IssueSyncBindingByID(ctx, bindingID)
	require.NoError(t, err)
	require.NotNil(t, got.LastCursorAt)
	assert.Equal(t, want, *got.LastCursorAt)
}

func assertCursorNil(ctx context.Context, t *testing.T, store db.Storage, bindingID int64) {
	t.Helper()
	got, err := store.IssueSyncBindingByID(ctx, bindingID)
	require.NoError(t, err)
	assert.Nil(t, got.LastCursorAt)
}

func issueTitleByID(ctx context.Context, t *testing.T, store *sqlitestore.Store, issueID int64) string {
	t.Helper()
	var title string
	require.NoError(t, store.QueryRowContext(ctx, `SELECT title FROM issues WHERE id = ?`, issueID).Scan(&title))
	return title
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

type testDiscardWriter struct{}

func (testDiscardWriter) Write(p []byte) (int, error) {
	return len(p), nil
}
