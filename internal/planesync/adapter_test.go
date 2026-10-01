package planesync

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

type adapterSession struct {
	project     Project
	states      []State
	items       []WorkItem
	beforeItems func(context.Context) error
	forRunCalls int
}

func (s *adapterSession) ForRun(ctx context.Context, _ Config) (Session, error) {
	s.forRunCalls++
	return s, ctx.Err()
}
func (s *adapterSession) Project(ctx context.Context, _ Config) (Project, error) {
	return s.project, ctx.Err()
}
func (s *adapterSession) States(ctx context.Context, _ Config) ([]State, error) {
	return s.states, ctx.Err()
}
func (s *adapterSession) WorkItems(ctx context.Context, _ Config) ([]WorkItem, error) {
	if s.beforeItems != nil {
		if err := s.beforeItems(ctx); err != nil {
			return nil, err
		}
	}
	return s.items, ctx.Err()
}
func newAdapterSession() *adapterSession {
	return &adapterSession{project: Project{ID: testProjectID, Name: "Example project", Identifier: "EX"}, states: []State{{ID: testStateID, Group: "started"}}, items: []WorkItem{testWorkItem()}}
}

func adapterStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	s, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}
func adapterBinding(t *testing.T, s db.Storage, c Config) db.IssueSyncBinding {
	t.Helper()
	p, err := s.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	b, err := s.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "plane", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: "Example project", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return b
}
func importedIssue(t *testing.T, s db.Storage, b db.IssueSyncBinding) db.Issue {
	t.Helper()
	mapping, err := s.ImportMappingBySource(context.Background(), b.ProjectID, b.SourceKey, "issue", "work-item:"+testItemID)
	require.NoError(t, err)
	i, err := s.IssueByID(context.Background(), *mapping.IssueID)
	require.NoError(t, err)
	return i
}

func TestAdapterImportsReplaysAndWorkflowGroupChanges(t *testing.T) {
	ctx := context.Background()
	store := adapterStore(t)
	c := testConfig()
	binding := adapterBinding(t, store, c)
	source := newAdapterSession()
	tracker := issuesync.NewProgressTracker()
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	var events []db.Event
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Progress: tracker, Clock: func() time.Time { return at }, EventSink: func(_ context.Context, _ int64, ev []db.Event) error { events = append(events, ev...); return nil }})
	result, err := runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, 1, result.Import.Created)
	require.Len(t, events, 1)
	initial := importedIssue(t, store, binding)
	require.Equal(t, "open", initial.Status)
	for _, group := range []string{"started", "completed", "cancelled", "started"} {
		at = at.Add(time.Minute)
		source.states[0].Group = group
		result, err = runner.RunOnce(ctx, binding.ID)
		require.NoError(t, err)
		got := importedIssue(t, store, binding)
		require.True(t, got.UpdatedAt.Equal(initial.UpdatedAt))
		if group == "completed" || group == "cancelled" {
			require.Equal(t, "closed", got.Status)
		} else {
			require.Equal(t, "open", got.Status)
		}
		if group == "started" && got.Revision == initial.Revision {
			require.Equal(t, 1, result.Import.Unchanged)
		} else {
			require.Equal(t, 1, result.Import.Updated)
		}
		require.Nil(t, tracker.Snapshot(binding.ID, at))
		require.Equal(t, at, result.Binding.LastCursorAt.UTC())
	}
	// Presentation changes at the same source timestamp remain source-owned.
	c.TitlePrefix = new(false)
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: binding.ProjectID, Provider: "plane", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: source.project.Name, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	got := importedIssue(t, store, binding)
	require.Equal(t, "Source task", got.Title)
	labels, err := store.LabelsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	require.Equal(t, "plane", labels[0].Label)
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: got.ID, Body: new("Local work"), Actor: "editor"})
	require.NoError(t, err)
	source.states[0].Group = "completed"
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	got = importedIssue(t, store, binding)
	require.Equal(t, "open", got.Status)
	require.Equal(t, "Local work", got.Body)
}

func TestRunnerPlaneDisplayNameRefreshCannotRestoreSupersededConfig(t *testing.T) {
	ctx := context.Background()
	store := adapterStore(t)
	c := statusConfig(t, "https://api.plane.so")
	binding := adapterBinding(t, store, c)
	source := newAdapterSession()
	source.project.Name = "Renamed project"
	source.beforeItems = func(ctx context.Context) error {
		operatorConfig := c
		operatorConfig.StatusSync = "one-way"
		raw, err := EncodeConfig(operatorConfig)
		require.NoError(t, err)
		_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
			ProjectID:       binding.ProjectID,
			Provider:        binding.Provider,
			SourceKey:       binding.SourceKey,
			RemoteID:        binding.RemoteID,
			DisplayName:     binding.DisplayName,
			Config:          raw,
			IntervalSeconds: 300,
		})
		return err
	}
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source})

	_, err := runner.RunOnce(ctx, binding.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged)

	stored, err := store.IssueSyncBindingByID(ctx, binding.ID)
	require.NoError(t, err)
	storedConfig, err := DecodeConfig(stored.Config)
	require.NoError(t, err)
	require.Equal(t, "one-way", storedConfig.StatusSync)
	require.Equal(t, binding.DisplayName, stored.DisplayName)
}

func TestAdapterFailsPreparationBeforeImportAndKeepsCursor(t *testing.T) {
	for _, kind := range []string{"unknown state", "partial content", "wrong project", "fetch failure", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := adapterStore(t)
			c := testConfig()
			binding := adapterBinding(t, store, c)
			source := newAdapterSession()
			second := testWorkItem()
			second.ID = testUserID
			source.items = append(source.items, second)
			switch kind {
			case "unknown state":
				source.items[1].StateID = testUserID
			case "partial content":
				source.items[1].DescriptionHTML = "invalid\x00content"
			case "wrong project":
				source.project.ID = testUserID
			case "fetch failure":
				source.beforeItems = func(context.Context) error { return errors.New("incomplete upstream read") }
			case "disabled":
				source.beforeItems = func(ctx context.Context) error {
					_, err := store.DisableIssueSyncBinding(ctx, binding.ProjectID)
					return err
				}
			}
			runner := NewRunner(RunnerConfig{Store: store, Fetcher: source})
			_, err := runner.RunOnce(ctx, binding.ID)
			require.Error(t, err)
			_, err = store.ImportMappingBySource(ctx, binding.ProjectID, binding.SourceKey, "issue", "work-item:"+testItemID)
			require.ErrorIs(t, err, db.ErrNotFound)
			stored, err := store.IssueSyncBindingByProject(ctx, binding.ProjectID)
			require.NoError(t, err)
			require.Nil(t, stored.LastCursorAt)
			status, err := store.IssueSyncStatusByProject(ctx, binding.ProjectID)
			require.NoError(t, err)
			require.Nil(t, status.SyncStartedAt)
		})
	}
}

func TestAdapterBlankProjectNameUsesIdentifier(t *testing.T) {
	store := adapterStore(t)
	binding := adapterBinding(t, store, testConfig())
	source := newAdapterSession()
	source.project.Name = " \t\n"
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source})
	result, err := runner.RunOnce(context.Background(), binding.ID)
	require.NoError(t, err)
	require.Equal(t, "EX", result.Binding.DisplayName)
	stored, err := store.IssueSyncBindingByProject(context.Background(), binding.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "EX", stored.DisplayName)
}

func TestAdapterImportsPriorityAndPreservesNewerLocalEdits(t *testing.T) {
	ctx := t.Context()
	store := adapterStore(t)
	binding := adapterBinding(t, store, testConfig())
	source := newAdapterSession()
	source.items[0].Priority = new(int64(1))
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source})
	_, err := runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	issue := importedIssue(t, store, binding)
	require.Equal(t, new(int64(1)), issue.Priority)
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "editor", SetPriority: new(int64(4))})
	require.NoError(t, err)
	_, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	local := importedIssue(t, store, binding)
	require.Equal(t, new(int64(4)), local.Priority)
	source.items[0].UpdatedAt = local.UpdatedAt.Add(time.Second)
	source.items[0].Priority = new(int64(0))
	_, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, new(int64(0)), importedIssue(t, store, binding).Priority)
	source.items[0].UpdatedAt = source.items[0].UpdatedAt.Add(time.Second)
	source.items[0].Priority = nil
	_, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Nil(t, importedIssue(t, store, binding).Priority)
}

func TestAdapterCutoffAndProgress(t *testing.T) {
	ctx := context.Background()
	store := adapterStore(t)
	c := testConfig()
	c.Since = "2026-09-01T10:00:00Z"
	binding := adapterBinding(t, store, c)
	source := newAdapterSession()
	source.items[0].UpdatedAt = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	tracker := issuesync.NewProgressTracker()
	at := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	source.beforeItems = func(context.Context) error {
		status, err := store.IssueSyncStatusByProject(ctx, binding.ProjectID)
		require.NoError(t, err)
		require.NotNil(t, status.SyncStartedAt)
		progress := tracker.Snapshot(binding.ID, *status.SyncStartedAt)
		require.NotNil(t, progress)
		require.Equal(t, "work-items", progress.Phase)
		return nil
	}
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Progress: tracker, Clock: func() time.Time { return at }})
	r, err := runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Zero(t, r.Import.Created)
	source.items[0].UpdatedAt = source.items[0].UpdatedAt.Add(time.Second)
	at = at.Add(time.Minute)
	r, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, 1, r.Import.Created)
}
