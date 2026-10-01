package issuesync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type statusTestAdapter struct {
	*testAdapter
	run *statusTestRun
}

func (*statusTestAdapter) Provider() string { return "notion" }
func (a *statusTestAdapter) OpenStatus(context.Context, db.IssueSyncBinding, time.Time) (StatusRun, error) {
	return a.run, nil
}

type statusTestRun struct {
	read  func(context.Context, db.IssueStatusMapping) (StatusObservation, error)
	write func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error)
}

func (s *statusTestRun) ReadStatus(ctx context.Context, m db.IssueStatusMapping) (StatusObservation, error) {
	return s.read(ctx, m)
}
func (s *statusTestRun) WriteStatus(ctx context.Context, m db.IssueStatusMapping, desired string, admit func() error) (StatusObservation, error) {
	return s.write(ctx, m, desired, admit)
}

func statusFixture(t *testing.T, n int) (*sqlitestore.Store, db.IssueSyncBinding, []db.IssueStatusMapping, time.Time) {
	return statusProviderFixture(t, n, "notion")
}
func statusProviderFixture(t *testing.T, n int, provider string) (*sqlitestore.Store, db.IssueSyncBinding, []db.IssueStatusMapping, time.Time) {
	t.Helper()
	s := testStore(t)
	b := testBinding(t, s, "example-project", provider)
	b, err := s.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 300})
	require.NoError(t, err)
	at := time.Now().UTC().Truncate(time.Millisecond)
	items := make([]db.ImportItem, n)
	for i := range items {
		items[i] = db.ImportItem{ExternalID: fmt.Sprintf("page-%d", i), Title: "Example task", Author: "worker", Status: "open", CreatedAt: at.Add(-time.Hour), UpdatedAt: at.Add(-time.Minute)}
	}
	_, _, err = s.ImportBatch(t.Context(), db.ImportBatchParams{ProjectID: b.ProjectID, Source: b.SourceKey, Actor: "notion-sync", Items: items})
	require.NoError(t, err)
	mappings := make([]db.IssueStatusMapping, n)
	for i := range items {
		m, err := s.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", items[i].ExternalID)
		require.NoError(t, err)
		mappings[i].Mapping = m
	}
	return s, b, mappings, at
}
func statusAdapter(_ db.IssueSyncBinding, run *statusTestRun) *statusTestAdapter {
	return &statusTestAdapter{run: run, testAdapter: &testAdapter{prepare: func(_ context.Context, binding db.IssueSyncBinding, _ time.Time) (Prepared, error) {
		return Prepared{Binding: binding, Batch: db.ImportBatchParams{Actor: "notion-sync"}}, nil
	}}}
}
func observed(state string, at time.Time) StatusObservation {
	return StatusObservation{RawStatus: new(state), Status: state, Version: at}
}
func privateMapping(t *testing.T, s *sqlitestore.Store, m db.IssueStatusMapping) db.ImportMappingExport {
	t.Helper()
	for r, err := range s.ExportImportMappings(t.Context(), db.ExportFilter{ProjectID: new(m.Mapping.ProjectID)}) {
		require.NoError(t, err)
		if r.ID == m.Mapping.ID {
			return r
		}
	}
	t.Fatal("missing mapping")
	return db.ImportMappingExport{}
}

func TestStatusRunnerDeliveryPrecedesBrokenContentAndAcknowledgesExactEvent(t *testing.T) {
	s, b, ms, at := statusFixture(t, 1)
	_, event, _, err := s.CloseIssue(t.Context(), *ms[0].Mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	calls := 0
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("closed", at), nil
	}, write: func(_ context.Context, m db.IssueStatusMapping, desired string, admit func() error) (StatusObservation, error) {
		require.Equal(t, event.UID, m.State.PendingEventUID)
		require.Equal(t, "closed", desired)
		require.NoError(t, admit())
		calls++
		return observed("closed", at), nil
	}}
	a := statusAdapter(b, run)
	a.prepare = func(context.Context, db.IssueSyncBinding, time.Time) (Prepared, error) {
		return Prepared{}, errors.New("unrelated content property unavailable")
	}
	r := NewRunner(RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at }})
	_, err = r.RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "content property")
	require.Equal(t, 1, calls)
	checkpoint := privateMapping(t, s, ms[0])
	require.Nil(t, checkpoint.PendingEventUID)
	require.NotNil(t, checkpoint.ObservedStatusAt)
	issue, err := s.IssueByID(t.Context(), *ms[0].Mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "closed", issue.Status)
}

func TestStatusRunnerReadSweepEmitsCommittedEventWithoutContent(t *testing.T) {
	s, b, ms, at := statusFixture(t, 1)
	sink := []db.Event{}
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("closed", at), nil
	}, write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
		t.Fatal("initial observation must never write")
		return StatusObservation{}, nil
	}}
	r := NewRunner(RunnerConfig{Store: s, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }, EventSink: func(_ context.Context, _ int64, events []db.Event) error { sink = append(sink, events...); return nil }})
	result, err := r.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, result.StatusUpdated)
	require.Len(t, sink, 1)
	require.Equal(t, "issue.updated", sink[0].Type)
	issue, err := s.IssueByID(t.Context(), *ms[0].Mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "closed", issue.Status)
	require.Nil(t, privateMapping(t, s, ms[0]).PendingEventUID)
}

func TestStatusRunnerOldReadbackCannotAcknowledgeNewerReopen(t *testing.T) {
	s, b, ms, at := statusFixture(t, 1)
	_, old, _, err := s.CloseIssue(t.Context(), *ms[0].Mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	var latest *db.Event
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("closed", at), nil
	}, write: func(ctx context.Context, m db.IssueStatusMapping, _ string, admit func() error) (StatusObservation, error) {
		require.Equal(t, old.UID, m.State.PendingEventUID)
		require.NoError(t, admit())
		_, latest, _, err = s.ReopenIssue(ctx, *m.Mapping.IssueID, "worker")
		require.NoError(t, err)
		return observed("closed", at), nil
	}}
	_, err = NewRunner(RunnerConfig{Store: s, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, latest.UID, *privateMapping(t, s, ms[0]).PendingEventUID)
	issue, err := s.IssueByID(t.Context(), *ms[0].Mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "open", issue.Status)
}

func TestStatusRunnerAmbiguityRetainsClaimAndDoesNotSendLaterWrites(t *testing.T) {
	s, b, ms, at := statusFixture(t, 2)
	for _, m := range ms {
		_, _, _, err := s.CloseIssue(t.Context(), *m.Mapping.IssueID, "done", "worker", "Completed task", nil)
		require.NoError(t, err)
	}
	writes := 0
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("open", at), nil
	}, write: func(_ context.Context, _ db.IssueStatusMapping, _ string, admit func() error) (StatusObservation, error) {
		require.NoError(t, admit())
		writes++
		return StatusObservation{}, &StatusError{Message: "write outcome unavailable", Ambiguous: true}
	}}
	_, err := NewRunner(RunnerConfig{Store: s, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "outcome unavailable")
	require.Equal(t, 1, writes)
	status, err := s.IssueSyncStatusByProject(t.Context(), b.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, status.SyncStartedAt)
	for _, m := range ms {
		require.NotNil(t, privateMapping(t, s, m).PendingEventUID)
	}
	_, claimed, err := s.ClaimIssueSyncBinding(t.Context(), b.ID, b.Provider, at.Add(time.Minute), at.Add(-29*time.Minute))
	require.NoError(t, err)
	require.False(t, claimed)
}

func TestStatusRunnerAdmissionRejectsSupersededIntentAndExpiredHorizon(t *testing.T) {
	for _, kind := range []string{"newer-event", "disable", "expired"} {
		t.Run(kind, func(t *testing.T) {
			s, b, ms, at := statusFixture(t, 1)
			_, _, _, err := s.CloseIssue(t.Context(), *ms[0].Mapping.IssueID, "done", "worker", "Completed task", nil)
			require.NoError(t, err)
			clock := at
			writes := 0
			run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
				return observed("open", at), nil
			}, write: func(ctx context.Context, m db.IssueStatusMapping, _ string, admit func() error) (StatusObservation, error) {
				switch kind {
				case "newer-event":
					_, _, _, err = s.ReopenIssue(ctx, *m.Mapping.IssueID, "worker")
				case "disable":
					_, err = s.DisableIssueSyncBinding(ctx, b.ProjectID)
				case "expired":
					clock = at.Add(26 * time.Minute)
				}
				require.NoError(t, err)
				err = admit()
				require.Error(t, err)
				if err == nil {
					writes++
				}
				return StatusObservation{}, err
			}}
			_, err = NewRunner(RunnerConfig{Store: s, Adapter: statusAdapter(b, run), Clock: func() time.Time { return clock }}).RunOnce(t.Context(), b.ID)
			if kind == "newer-event" {
				require.NoError(t, err, "superseded intent waits for the next lap")
			} else {
				require.Error(t, err)
			}
			require.Zero(t, writes)
			require.NotNil(t, privateMapping(t, s, ms[0]).PendingEventUID)
		})
	}
}

func TestStatusRunnerFailedPrefixCannotStarveLaterPendingAcrossRestart(t *testing.T) {
	s, b, ms, at := statusFixture(t, 101)
	for _, m := range ms {
		_, _, _, err := s.CloseIssue(t.Context(), *m.Mapping.IssueID, "done", "worker", "Completed task", nil)
		require.NoError(t, err)
	}
	var visited []int64
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		t.Fatal("pending mappings have no inward authority")
		return StatusObservation{}, nil
	}, write: func(_ context.Context, m db.IssueStatusMapping, _ string, admit func() error) (StatusObservation, error) {
		require.NoError(t, admit())
		visited = append(visited, m.Mapping.ID)
		if m.Mapping.ID != ms[100].Mapping.ID {
			return StatusObservation{}, &StatusError{Message: "provider unavailable", HTTPStatus: 503}
		}
		return observed("closed", at), nil
	}}
	config := RunnerConfig{Store: s, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }}
	_, err := NewRunner(config).RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "provider unavailable")
	require.Len(t, visited, 100)
	b, err = s.IssueSyncBindingByID(t.Context(), b.ID)
	require.NoError(t, err)
	scan, err := db.DecodeIssueStatusScan(b.Config)
	require.NoError(t, err)
	require.Equal(t, ms[99].Mapping.ID, scan.Pending.After)
	require.Equal(t, ms[100].Mapping.ID, scan.Pending.Through)
	// A fresh runner resumes private progress, independent of the content cursor.
	_, err = NewRunner(config).RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Len(t, visited, 101)
	require.Nil(t, privateMapping(t, s, ms[100]).PendingEventUID)
	require.NotNil(t, privateMapping(t, s, ms[0]).PendingEventUID)
}

func TestStatusRunnerErrorsDoNotMaskContentOrOverwriteLocalIntent(t *testing.T) {
	s, b, ms, at := statusFixture(t, 1)
	_, event, _, err := s.CloseIssue(t.Context(), *ms[0].Mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return StatusObservation{}, errors.New("status read unavailable")
	}, write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
		return StatusObservation{}, &StatusError{Message: "status write forbidden", HTTPStatus: 403}
	}}
	a := statusAdapter(b, run)
	a.prepare = func(_ context.Context, b db.IssueSyncBinding, _ time.Time) (Prepared, error) {
		p := testPrepared(b, at)
		p.Batch.Items[0].ExternalID = ms[0].Mapping.ExternalID
		p.Batch.Items[0].Title = "Updated upstream title"
		p.Batch.Items[0].UpdatedAt = at.Add(time.Hour)
		return p, nil
	}
	result, err := NewRunner(RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "status write forbidden")
	require.Equal(t, 1, result.Import.Updated)
	issue, err := s.IssueByID(t.Context(), *ms[0].Mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "Updated upstream title", issue.Title)
	require.Equal(t, "closed", issue.Status)
	require.Equal(t, event.UID, *privateMapping(t, s, ms[0]).PendingEventUID)
	b, err = s.IssueSyncBindingByID(t.Context(), b.ID)
	require.NoError(t, err)
	require.Nil(t, b.LastCursorAt, "a failed pass must not be reported as overall success")
}

func TestStatusRunnerBlockedErrorsRemainVisibleWhileContentAdvances(t *testing.T) {
	s, b, ms, at := statusFixture(t, 2)
	_, pendingEvent, _, err := s.CloseIssue(t.Context(), *ms[0].Mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	reads, writes := 0, 0
	run := &statusTestRun{
		read: func(_ context.Context, m db.IssueStatusMapping) (StatusObservation, error) {
			require.Equal(t, ms[1].Mapping.ID, m.Mapping.ID)
			reads++
			return StatusObservation{}, &StatusError{Message: "status read blocked", HTTPStatus: 403, Blocked: true}
		},
		write: func(_ context.Context, m db.IssueStatusMapping, desired string, admit func() error) (StatusObservation, error) {
			require.Equal(t, ms[0].Mapping.ID, m.Mapping.ID)
			require.Equal(t, pendingEvent.UID, m.State.PendingEventUID)
			require.Equal(t, "closed", desired)
			require.NoError(t, admit())
			writes++
			return StatusObservation{}, &StatusError{Message: "status write blocked", HTTPStatus: 403, Blocked: true}
		},
	}
	result, err := NewRunner(RunnerConfig{Store: s, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "status write blocked")
	require.NotEmpty(t, result.Status.LastError)
	require.Contains(t, result.Status.LastError, "status write blocked")
	require.Nil(t, result.Status.LastSuccessAt, "blocked delivery must not report overall success")
	require.Equal(t, 1, reads)
	require.Equal(t, 1, writes)
	require.Zero(t, result.StatusUpdated)
	require.Equal(t, pendingEvent.UID, *privateMapping(t, s, ms[0]).PendingEventUID)
	b, err = s.IssueSyncBindingByID(t.Context(), b.ID)
	require.NoError(t, err)
	require.NotNil(t, b.LastCursorAt, "blocked per-item status errors must not fail the content sync")
	require.Equal(t, at, *b.LastCursorAt)
}

func TestStatusRunnerRetainsClaimWhenAmbiguityFollowsOrdinaryFailure(t *testing.T) {
	s, b, ms, at := statusFixture(t, 2)
	for _, m := range ms {
		_, _, _, err := s.CloseIssue(t.Context(), *m.Mapping.IssueID, "done", "worker", "Completed task", nil)
		require.NoError(t, err)
	}
	calls := 0
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("open", at), nil
	}, write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
		calls++
		if calls == 1 {
			return StatusObservation{}, &StatusError{Message: "first issue forbidden", HTTPStatus: 403}
		}
		return StatusObservation{}, &StatusError{Message: "second write outcome unknown", Ambiguous: true}
	}}
	_, err := NewRunner(RunnerConfig{Store: s, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "outcome unknown")
	status, err := s.IssueSyncStatusByProject(t.Context(), b.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, status.SyncStartedAt)
}

type locatorTestRun struct {
	*statusTestRun
	pages       *[]int
	locatorPage []db.IssueStatusLocator
}

func (r *locatorTestRun) Locators(_ context.Context, page int) ([]db.IssueStatusLocator, int, error) {
	*r.pages = append(*r.pages, page)
	if page == 1 {
		if r.locatorPage != nil {
			return r.locatorPage, 2, nil
		}
		return []db.IssueStatusLocator{{ExternalID: "issue-id:123", LegacyExternalIDs: []string{"issue:example-node"}, Locator: "7"}}, 2, nil
	}
	return nil, 0, nil
}
func TestStatusRunnerBackfillsVerifiedLocatorBeforeDeliveryAndKeepsEnumerationProgress(t *testing.T) {
	s, b, ms, at := statusProviderFixture(t, 1, "github")
	_, err := s.ExecContext(t.Context(), `UPDATE import_mappings SET external_id='issue:example-node' WHERE id=$1`, ms[0].Mapping.ID)
	require.NoError(t, err)
	_, _, _, err = s.CloseIssue(t.Context(), *ms[0].Mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	var pages []int
	writes := 0
	run := &locatorTestRun{pages: &pages, statusTestRun: &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("closed", at), nil
	}, write: func(_ context.Context, m db.IssueStatusMapping, _ string, admit func() error) (StatusObservation, error) {
		require.Equal(t, "issue-id:123", m.Mapping.ExternalID)
		require.Equal(t, "7", m.State.RemoteLocator)
		require.NoError(t, admit())
		writes++
		return observed("closed", at), nil
	}}}
	a := &locatorTestAdapter{testAdapter: &testAdapter{prepare: func(_ context.Context, b db.IssueSyncBinding, _ time.Time) (Prepared, error) {
		return Prepared{Binding: b, Batch: db.ImportBatchParams{Actor: "github-sync"}}, nil
	}}, run: run}
	config := RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at }}
	_, err = NewRunner(config).RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, writes)
	require.Equal(t, []int{1}, pages)
	_, err = NewRunner(config).RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, []int{1, 2}, pages)
}

func TestStatusRunnerLocatorBackfillSkipsDeletedLocalMappingsAndAdvancesPage(t *testing.T) {
	s, b, ms, at := statusProviderFixture(t, 2, "github")
	for i, externalID := range []string{"issue:deleted-node", "issue:active-node"} {
		_, err := s.ExecContext(t.Context(), `UPDATE import_mappings SET external_id=$1 WHERE id=$2`, externalID, ms[i].Mapping.ID)
		require.NoError(t, err)
	}
	_, _, _, err := s.SoftDeleteIssue(t.Context(), *ms[0].Mapping.IssueID, "worker")
	require.NoError(t, err)
	var pages []int
	run := &locatorTestRun{
		pages:       &pages,
		locatorPage: []db.IssueStatusLocator{{ExternalID: "issue-id:deleted", LegacyExternalIDs: []string{"issue:deleted-node"}, Locator: "17"}, {ExternalID: "issue-id:active", LegacyExternalIDs: []string{"issue:active-node"}, Locator: "18"}},
		statusTestRun: &statusTestRun{
			read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
				return observed("open", at), nil
			},
			write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
				t.Fatal("no active mapping has pending intent")
				return StatusObservation{}, nil
			},
		},
	}
	a := &locatorTestAdapter{
		testAdapter: &testAdapter{prepare: func(_ context.Context, binding db.IssueSyncBinding, _ time.Time) (Prepared, error) {
			return Prepared{Binding: binding, Batch: db.ImportBatchParams{Actor: "github-sync"}}, nil
		}},
		run: run,
	}
	_, err = NewRunner(RunnerConfig{Store: s, Adapter: a, Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, []int{1}, pages)
	active := privateMapping(t, s, ms[1])
	require.Equal(t, "issue-id:active", active.ExternalID)
	require.NotNil(t, active.RemoteLocator)
	require.Equal(t, "18", *active.RemoteLocator)
	b, err = s.IssueSyncBindingByID(t.Context(), b.ID)
	require.NoError(t, err)
	scan, err := db.DecodeIssueStatusScan(b.Config)
	require.NoError(t, err)
	require.Equal(t, 2, scan.LocatorPage)
}

type locatorTestAdapter struct {
	*testAdapter
	run *locatorTestRun
}

func (*locatorTestAdapter) Provider() string { return "github" }
func (a *locatorTestAdapter) OpenStatus(context.Context, db.IssueSyncBinding, time.Time) (StatusRun, error) {
	return a.run, nil
}

type editedSuccessStore struct {
	*sqlitestore.Store
	edit func(context.Context)
}

func (s *editedSuccessStore) RecordIssueSyncSuccess(ctx context.Context, p db.IssueSyncSuccessParams) (db.IssueSyncStatus, error) {
	s.edit(ctx)
	return s.Store.RecordIssueSyncSuccess(ctx, p)
}
func TestStatusRunnerSuccessCannotAdvanceCursorAfterOperatorEdit(t *testing.T) {
	s, b, _, at := statusFixture(t, 1)
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		return observed("open", at), nil
	}, write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
		t.Fatal("no pending intent")
		return StatusObservation{}, nil
	}}
	wrapped := &editedSuccessStore{Store: s, edit: func(ctx context.Context) {
		_, err := s.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: []byte(`{"status_sync":"two-way","title_prefix":false}`), IntervalSeconds: 300})
		require.NoError(t, err)
	}}
	_, err := NewRunner(RunnerConfig{Store: wrapped, Adapter: statusAdapter(b, run), Clock: func() time.Time { return at }}).RunOnce(t.Context(), b.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged)
	current, err := s.IssueSyncBindingByID(t.Context(), b.ID)
	require.NoError(t, err)
	require.Nil(t, current.LastCursorAt, "old pass cannot advance a reset cursor for the edited configuration")
}
