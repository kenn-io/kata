package twentysync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/issuesync"
)

type adapterSource struct {
	workspace             Workspace
	schema                Schema
	items                 []Task
	contentErr            error
	beforeTasks           func(context.Context) error
	beforeWrite           func(context.Context) error
	tasksConfig           Config
	readCount, writeCount int
}

func (s *adapterSource) ForRun(ctx context.Context, _ Config) (Session, error) { return s, ctx.Err() }
func (s *adapterSource) Workspace(ctx context.Context, _ Config) (Workspace, error) {
	return s.workspace, ctx.Err()
}
func (s *adapterSource) Schema(ctx context.Context, _ Config) (Schema, error) {
	return s.schema, ctx.Err()
}
func (s *adapterSource) Tasks(ctx context.Context, c Config) ([]Task, error) {
	s.tasksConfig = c
	if s.beforeTasks != nil {
		if err := s.beforeTasks(ctx); err != nil {
			return nil, err
		}
	}
	return s.items, s.contentErr
}
func (s *adapterSource) ReadStatus(_ context.Context, c Config, id string) (issuesync.StatusObservation, error) {
	s.readCount++
	for _, task := range s.items {
		if task.ID == id {
			status, reason, err := ClassifyStatus(c, task.Status)
			o := issuesync.StatusObservation{RawStatus: task.Status, Status: status, ClosedReason: reason, Version: task.UpdatedAt}
			if status == "closed" {
				o.ClosedAt = new(o.Version)
			}
			return o, err
		}
	}
	return issuesync.StatusObservation{}, blockedStatus("task unavailable")
}
func (s *adapterSource) WriteStatus(ctx context.Context, c Config, id, desired string, admit func() error) (issuesync.StatusObservation, error) {
	if s.beforeWrite != nil {
		if err := s.beforeWrite(ctx); err != nil {
			return issuesync.StatusObservation{}, err
		}
	}
	if err := admit(); err != nil {
		return issuesync.StatusObservation{}, err
	}
	s.writeCount++
	for i := range s.items {
		if s.items[i].ID == id {
			target := c.ClosedStatus
			if target == "" {
				target = "DONE"
			}
			if desired == "open" {
				target = c.OpenStatus
				if target == "" {
					target = "TODO"
				}
			}
			s.items[i].Status = &target
		}
	}
	return s.ReadStatus(ctx, c, id)
}

func adapterStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}
func adapterBinding(t *testing.T, store db.Storage, c Config) db.IssueSyncBinding {
	t.Helper()
	p, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	b, err := store.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "twenty", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: "Example workspace", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return b
}
func newAdapterSource(at time.Time) *adapterSource {
	row := testTask()
	row.Status = new("TODO")
	row.CreatedAt = at.Add(-2 * time.Hour)
	row.UpdatedAt = at.Add(-time.Hour)
	return &adapterSource{workspace: Workspace{ID: workspaceID, DisplayName: "Example workspace"}, schema: Schema{StatusOptions: []string{"TODO", "IN_PROGRESS", "DONE"}}, items: []Task{row}}
}
func importedIssue(t *testing.T, store db.Storage, b db.IssueSyncBinding) db.Issue {
	t.Helper()
	m, err := store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "task:"+taskID)
	require.NoError(t, err)
	i, err := store.IssueByID(t.Context(), *m.IssueID)
	require.NoError(t, err)
	return i
}

func TestAdapterImportsAndReplaysThroughSharedRunner(t *testing.T) {
	store := adapterStore(t)
	c := testConfig()
	b := adapterBinding(t, store, c)
	at := time.Now().UTC().Truncate(time.Millisecond)
	source := newAdapterSource(at)
	var events []db.Event
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return at }, EventSink: func(_ context.Context, _ int64, e []db.Event) error { events = append(events, e...); return nil }})
	result, err := runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, result.Import.Created)
	require.Len(t, events, 1)
	require.Equal(t, "open", importedIssue(t, store, b).Status)
	at = at.Add(time.Minute)
	result, err = runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, result.Import.Unchanged)
	require.Equal(t, at, result.Binding.LastCursorAt.UTC())
	source.items[0].Status = new("DONE")
	source.items[0].UpdatedAt = at
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", importedIssue(t, store, b).Status)
}

func TestAdapterCutoffFailureAndDisableRetainNativeData(t *testing.T) {
	store := adapterStore(t)
	c := testConfig()
	at := time.Now().UTC().Truncate(time.Millisecond)
	c.Since = "2026-01-02T03:04:05+01:00"
	b := adapterBinding(t, store, c)
	source := newAdapterSource(at)
	source.items = nil
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return at }})
	result, err := runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	// The session applies the saved cutoff before content checks.
	require.Equal(t, "2026-01-02T02:04:05Z", source.tasksConfig.Since)
	require.Zero(t, result.Import.Created)
	source.contentErr = errors.New("task collection incomplete")
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "incomplete")
	stored, err := store.IssueSyncBindingByID(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, result.Binding.LastCursorAt.UTC(), stored.LastCursorAt.UTC())
	source.contentErr = nil
	source.beforeTasks = func(ctx context.Context) error { _, err := store.DisableIssueSyncBinding(ctx, b.ProjectID); return err }
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(t.Context(), b.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncNotEnabled)
}

func TestAdapterTwoWayDeliveryPrecedesBrokenContent(t *testing.T) {
	store := adapterStore(t)
	c := testConfig()
	c.StatusSync = "two-way"
	b := adapterBinding(t, store, c)
	at := time.Now().UTC().Truncate(time.Millisecond)
	source := newAdapterSource(at)
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return at }})
	_, err := runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Zero(t, source.writeCount)
	issue := importedIssue(t, store, b)
	_, _, _, err = store.CloseIssue(t.Context(), issue.ID, "done", "worker", "Completed imported task", nil)
	require.NoError(t, err)
	source.contentErr = errors.New("task Markdown unavailable")
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "Markdown")
	require.Equal(t, 1, source.writeCount)
	require.Equal(t, "DONE", *source.items[0].Status)
	count, err := store.CountPendingIssueStatuses(t.Context(), b.ID)
	require.NoError(t, err)
	require.Zero(t, count)
	_, _, _, err = store.ReopenIssue(t.Context(), issue.ID, "worker")
	require.NoError(t, err)
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(t.Context(), b.ID)
	require.Error(t, err)
	require.Equal(t, 2, source.writeCount)
	require.Equal(t, "TODO", *source.items[0].Status)
}

func TestAdapterWorkspaceMismatchRejectsStatusBeforeContent(t *testing.T) {
	store := adapterStore(t)
	c := testConfig()
	c.StatusSync = "two-way"
	b := adapterBinding(t, store, c)
	at := time.Now().UTC().Truncate(time.Millisecond)
	source := newAdapterSource(at)
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return at }})
	_, err := runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	issue := importedIssue(t, store, b)
	_, _, _, err = store.CloseIssue(t.Context(), issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	source.workspace.ID = taskID
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(t.Context(), b.ID)
	require.Error(t, err)
	require.Zero(t, source.writeCount)
	require.Zero(t, source.readCount)
	count, err := store.CountPendingIssueStatuses(t.Context(), b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestAdapterStatusAdmissionFencesModeChanges(t *testing.T) {
	store := adapterStore(t)
	c := testConfig()
	c.StatusSync = "two-way"
	b := adapterBinding(t, store, c)
	at := time.Now().UTC().Truncate(time.Millisecond)
	source := newAdapterSource(at)
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return at }})
	_, err := runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	issue := importedIssue(t, store, b)
	_, _, _, err = store.CloseIssue(t.Context(), issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	source.beforeWrite = func(ctx context.Context) error {
		c.StatusSync = "one-way"
		raw, err := EncodeConfig(c)
		require.NoError(t, err)
		_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
		return err
	}
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(t.Context(), b.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged)
	require.Zero(t, source.writeCount)
}
