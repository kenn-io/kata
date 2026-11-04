package githubsync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

type statusRunnerFetcher struct {
	*fakeRunnerFetcher
	writes   int
	number   int
	external string
}

func (s *statusRunnerFetcher) ReadStatus(_ context.Context, _ Config, id string, number int) (issuesync.StatusObservation, error) {
	s.external = id
	s.number = number
	return issuesync.StatusObservation{RawStatus: new("closed"), Status: "closed", Version: *s.issues[0].UpdatedAt, Locator: "1"}, nil
}
func (s *statusRunnerFetcher) WriteStatus(ctx context.Context, c Config, id string, number int, _ string, admit func() error) (issuesync.StatusObservation, error) {
	if err := admit(); err != nil {
		return issuesync.StatusObservation{}, err
	}
	s.writes++
	return s.ReadStatus(ctx, c, id, number)
}
func TestGitHubStatusMetadataRefreshPreservesTwoWayMode(t *testing.T) {
	h := newRunnerHarness(t)
	cfg, err := DecodeConfig(h.binding.Config)
	require.NoError(t, err)
	cfg.StatusSync = "two-way"
	_, claimed, err := h.db.ClaimIssueSyncBinding(h.ctx, h.binding.ID, "github", h.now, h.now.Add(-DefaultStaleLockTTL))
	require.NoError(t, err)
	require.True(t, claimed)
	_, updated, err := (&adapter{config: h.runner.config}).refreshRepository(h.ctx, h.binding, cfg, h.fetcher.repo, h.now)
	require.NoError(t, err)
	require.Equal(t, "two-way", updated.StatusSync)
}
func TestGitHubStatusDeliveryRunsBeforeParentImportFailure(t *testing.T) {
	h := newRunnerHarness(t)
	h.fetcher.issues = []Issue{testIssue(101, 1, "Example task", h.now.Add(-1))}
	_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
	require.NoError(t, err)
	cfg, err := DecodeConfig(h.binding.Config)
	require.NoError(t, err)
	cfg.StatusSync = "two-way"
	raw, err := EncodeConfig(cfg)
	require.NoError(t, err)
	b, err := h.db.UpsertIssueSyncBinding(h.ctx, db.UpsertIssueSyncBindingParams{ProjectID: h.binding.ProjectID, Provider: "github", SourceKey: h.binding.SourceKey, RemoteID: h.binding.RemoteID, DisplayName: h.binding.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	mapping, err := h.db.ImportMappingBySource(h.ctx, b.ProjectID, b.SourceKey, "issue", "issue-id:101")
	require.NoError(t, err)
	_, _, _, err = h.db.CloseIssue(h.ctx, *mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	_, err = h.db.ExecContext(h.ctx, `UPDATE import_mappings SET remote_locator='1' WHERE id=$1`, mapping.ID)
	require.NoError(t, err)
	fetcher := &statusRunnerFetcher{fakeRunnerFetcher: h.fetcher}
	fetcher.parentMapErr = errors.New("parents unavailable")
	h.runner.config.Fetcher = fetcher
	// Use actual store private status interfaces, rather than import instrumentation.
	h.runner.config.Store = h.db
	_, err = h.runner.RunOnce(h.ctx, b.ID)
	require.ErrorContains(t, err, "parents unavailable")
	require.Equal(t, 1, fetcher.writes)
	require.Equal(t, 1, fetcher.number)
	require.Equal(t, "issue-id:101", fetcher.external)
}

func TestGitHubStatusNewImportsCaptureVerifiedAPIIdentifier(t *testing.T) {
	h := newRunnerHarness(t)
	h.fetcher.issues = []Issue{testIssue(101, 1, "Example task", h.now.Add(-1))}
	fetcher := &statusRunnerFetcher{fakeRunnerFetcher: h.fetcher}
	h.runner.config.Fetcher = fetcher
	h.runner.config.Store = h.db
	cfg, err := DecodeConfig(h.binding.Config)
	require.NoError(t, err)
	cfg.StatusSync = "two-way"
	raw, err := EncodeConfig(cfg)
	require.NoError(t, err)
	b, err := h.db.UpsertIssueSyncBinding(h.ctx, db.UpsertIssueSyncBindingParams{ProjectID: h.binding.ProjectID, Provider: "github", SourceKey: h.binding.SourceKey, RemoteID: h.binding.RemoteID, DisplayName: h.binding.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	result, err := h.runner.RunOnce(h.ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, result.Import.Created)
	var found bool
	for row, err := range h.db.ExportImportMappings(h.ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		if row.ExternalID == "issue-id:101" {
			found = true
			require.NotNil(t, row.RemoteLocator)
			require.Equal(t, "1", *row.RemoteLocator)
			require.Nil(t, row.PendingEventUID)
		}
	}
	require.True(t, found)
}

func TestGitHubMetadataRefreshIgnoresPrivateScanProgress(t *testing.T) {
	h := newRunnerHarness(t)
	_, claimed, err := h.db.ClaimIssueSyncBinding(h.ctx, h.binding.ID, "github", h.now, h.now.Add(-DefaultStaleLockTTL))
	require.NoError(t, err)
	require.True(t, claimed)
	cfg, err := DecodeConfig(h.binding.Config)
	require.NoError(t, err)
	a := &adapter{config: h.runner.config}
	b, cfg, err := a.refreshRepository(h.ctx, h.binding, cfg, h.fetcher.repo, h.now)
	require.NoError(t, err)
	guard := db.IssueSyncImportGuard{BindingID: b.ID, Provider: b.Provider, StartedAt: h.now, BindingUpdatedAt: new(b.UpdatedAt)}
	b, err = h.db.UpdateIssueStatusScan(h.ctx, guard, db.IssueStatusScanState{Sweep: db.IssueStatusScanCursor{After: 1, Through: 2}})
	require.NoError(t, err)
	refreshed, _, err := a.refreshRepository(h.ctx, b, cfg, h.fetcher.repo, h.now)
	require.NoError(t, err)
	require.Equal(t, b.UpdatedAt, refreshed.UpdatedAt, "scan progress alone must not refresh provider metadata")
}
