package issuesync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestStatusRunnerOneWayKeepsContentDrivenStatus(t *testing.T) {
	s, b, ms, at := statusFixture(t, 1)
	b, err := s.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: []byte(`{"status_sync":"one-way"}`), IntervalSeconds: 300})
	require.NoError(t, err)
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		t.Fatal("one-way bindings take status from content imports")
		return StatusObservation{}, nil
	}, write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
		t.Fatal("one-way bindings never write status")
		return StatusObservation{}, nil
	}}
	a := statusAdapter(b, run)
	a.prepare = func(_ context.Context, b db.IssueSyncBinding, _ time.Time) (Prepared, error) {
		p := testPrepared(b, at)
		p.Batch.Items[0].ExternalID = ms[0].Mapping.ExternalID
		p.Batch.Items[0].Status = "closed"
		p.Batch.Items[0].ClosedReason = new("done")
		p.Batch.Items[0].ClosedAt = new(at)
		p.Batch.Items[0].UpdatedAt = at.Add(time.Hour)
		return p, nil
	}
	_, err = NewRunner(RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	issue, err := s.IssueByID(t.Context(), *ms[0].Mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "closed", issue.Status)
}

type failingScanStore struct {
	*sqlitestore.Store
}

func (*failingScanStore) UpdateIssueStatusScan(context.Context, db.IssueSyncImportGuard, db.IssueStatusScanState) (db.IssueSyncBinding, error) {
	return db.IssueSyncBinding{}, db.ErrIssueSyncBindingChanged
}

func TestStatusRunnerFailedScanUpdateReleasesClaim(t *testing.T) {
	s, b, _, at := statusProviderFixture(t, 1, "github")
	var pages []int
	run := &locatorTestRun{pages: &pages, statusTestRun: &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("open", at), nil
	}, write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
		t.Fatal("no pending intent")
		return StatusObservation{}, nil
	}}}
	a := &locatorTestAdapter{testAdapter: &testAdapter{prepare: func(_ context.Context, b db.IssueSyncBinding, _ time.Time) (Prepared, error) {
		return Prepared{Binding: b, Batch: db.ImportBatchParams{Actor: "github-sync"}}, nil
	}}, run: run}
	_, err := NewRunner(RunnerConfig{Store: &failingScanStore{Store: s}, Adapter: a, Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged)
	status, err := s.IssueSyncStatusByProject(t.Context(), b.ProjectID)
	require.NoError(t, err)
	require.Nil(t, status.SyncStartedAt, "the run must release its claim")
	require.NotNil(t, status.LastError)
}

func TestStatusRunnerSupersededIntentIsNotAFailure(t *testing.T) {
	s, b, ms, at := statusFixture(t, 1)
	_, _, _, err := s.CloseIssue(t.Context(), *ms[0].Mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("open", at), nil
	}, write: func(ctx context.Context, m db.IssueStatusMapping, _ string, admit func() error) (StatusObservation, error) {
		_, _, _, err := s.ReopenIssue(ctx, *m.Mapping.IssueID, "worker")
		require.NoError(t, err)
		return StatusObservation{}, admit()
	}}
	result, err := NewRunner(RunnerConfig{Store: s, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.NoError(t, err, "a reopen that races a queued close is delivered on the next lap")
	require.NotNil(t, result.Status.LastSuccessAt)
	require.NotNil(t, privateMapping(t, s, ms[0]).PendingEventUID)
}

func TestStatusRunnerTwoWayDoesNotCapContentRun(t *testing.T) {
	s, b, _, at := statusFixture(t, 1)
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("open", at), nil
	}, write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
		t.Fatal("no pending intent")
		return StatusObservation{}, nil
	}}
	a := statusAdapter(b, run)
	a.prepare = func(ctx context.Context, b db.IssueSyncBinding, _ time.Time) (Prepared, error) {
		_, bounded := ctx.Deadline()
		require.False(t, bounded, "content work keeps the provider's own run timeout")
		return Prepared{Binding: b, Batch: db.ImportBatchParams{Actor: "notion-sync"}}, nil
	}
	_, err := NewRunner(RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
}

func TestStatusRunnerInvalidMappingDoesNotStopTheSweep(t *testing.T) {
	for name, corrupt := range map[string]string{
		"missing pending event": `UPDATE import_mappings SET pending_event_uid='01HZZZZZZZZZZZZZZZZZZZZZ99' WHERE id=$1`,
		"invalid observation":   `UPDATE import_mappings SET observed_status_at='invalid' WHERE id=$1`,
	} {
		t.Run(name, func(t *testing.T) {
			s, b, ms, at := statusFixture(t, 2)
			_, err := s.ExecContext(t.Context(), corrupt, ms[0].Mapping.ID)
			require.NoError(t, err)
			var read []int64
			run := &statusTestRun{read: func(_ context.Context, m db.IssueStatusMapping) (StatusObservation, error) {
				read = append(read, m.Mapping.ID)
				return observed("closed", at), nil
			}, write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
				t.Fatal("an unloadable mapping is never written")
				return StatusObservation{}, nil
			}}
			result, err := NewRunner(RunnerConfig{Store: s, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
			require.Error(t, err)
			require.True(t, IsBlockedStatusWarning(err), "invalid local state is reported without failing content")
			require.Equal(t, []int64{ms[1].Mapping.ID}, read)
			require.Equal(t, 1, result.StatusUpdated)
		})
	}
}
