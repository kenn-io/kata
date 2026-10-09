package todoistsync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/issuesync"
)

// Contract: durable local intent is delivered once; restart/replay cannot echo it.
func TestRunnerDurableCloseReopenAndReplay(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := newAPIFixture(t)
	c := f.cfg()
	c.StatusSync = "two-way"
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "todoist", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), Config: raw, DisplayName: "Example tasks", IntervalSeconds: 300})
	require.NoError(t, err)
	newRunner := func() *issuesync.Runner {
		return NewRunner(RunnerConfig{Store: store, Fetcher: f.client(), Clock: func() time.Time { return f.now }})
	}
	runner := newRunner()
	_, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	mapping, err := store.ImportMappingBySource(ctx, p.ID, c.SourceKey(), "issue", "task:"+f.row.ID)
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
	result, err := runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, 1, result.Import.Unchanged)
	require.Empty(t, f.posts)
	// Contract: the latest accepted local transition supersedes undelivered intent.
	_, _, _, err = store.CloseIssueWithEvents(ctx, *mapping.IssueID, "done", "worker", "Completed standalone task", nil)
	require.NoError(t, err)
	_, _, _, err = store.ReopenIssue(ctx, *mapping.IssueID, "worker")
	require.NoError(t, err)
	_, err = newRunner().RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Empty(t, f.posts)
	_, _, changed, err := store.CloseIssueWithEvents(ctx, *mapping.IssueID, "done", "worker", "Completed standalone task", nil)
	require.NoError(t, err)
	require.True(t, changed)
	// Reconstruct the worker before delivery, exercising persisted intent after restart.
	runner = newRunner()
	_, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Len(t, f.posts, 1)
	require.True(t, f.row.Checked)
	_, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Len(t, f.posts, 1)
	_, _, changed, err = store.ReopenIssue(ctx, *mapping.IssueID, "worker")
	require.NoError(t, err)
	require.True(t, changed)
	_, err = newRunner().RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Len(t, f.posts, 2)
	require.False(t, f.row.Checked)
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "open", issue.Status)
}
func TestAdapterRejectsChangedBindingBeforeReads(t *testing.T) {
	ctx := context.Background()
	f := newAPIFixture(t)
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	raw, err := EncodeConfig(f.cfg())
	require.NoError(t, err)
	_, err = NewAdapter(store, f.client()).Prepare(ctx, db.IssueSyncBinding{Provider: "todoist", Config: raw, SourceKey: "foreign", RemoteID: f.cfg().RemoteID()}, f.now)
	require.Error(t, err)
	require.Zero(t, f.reads)
}

func newRunnerFixture(t *testing.T, mode string) (*sqlitestore.Store, db.IssueSyncBinding, *apiFixture) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := newAPIFixture(t)
	c := f.cfg()
	c.StatusSync = mode
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "todoist", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), Config: raw, DisplayName: "Example tasks", IntervalSeconds: 300})
	require.NoError(t, err)
	return store, binding, f
}

func (f *apiFixture) runner(store db.Storage) *issuesync.Runner {
	return NewRunner(RunnerConfig{Store: store, Fetcher: f.client(), Clock: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }})
}

// Contract: after a successful run, completion history resumes from the saved
// cursor with the shared two-minute overlap instead of replaying from the floor.
func TestRunnerHistoryResumesFromLastCursor(t *testing.T) {
	ctx := context.Background()
	store, binding, f := newRunnerFixture(t, "one-way")
	_, err := f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	binding, err = store.IssueSyncBindingByProject(ctx, binding.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, binding.LastCursorAt)
	f.completeInTodoist()
	f.queries = nil
	_, err = f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	var since []string
	for _, q := range f.queries {
		if q.Has("since") {
			since = append(since, q.Get("since"))
		}
	}
	require.Equal(t, []string{binding.LastCursorAt.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)}, since)
	mapping, err := store.ImportMappingBySource(ctx, binding.ProjectID, binding.SourceKey, "issue", "task:"+f.row.ID)
	require.NoError(t, err)
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "closed", issue.Status)
}

// Contract: once a completion is verified, later sweeps re-read only the task's
// active state; they neither replay completion history nor rescope per task.
func TestRunnerSweepOfCompletedTaskSkipsHistoryReplay(t *testing.T) {
	ctx := context.Background()
	store, binding, f := newRunnerFixture(t, "two-way")
	_, err := f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	f.completeInTodoist()
	_, err = f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	f.mu.Lock()
	f.paths = nil
	f.now = f.now.Add(time.Hour)
	f.mu.Unlock()
	_, err = f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, 1, f.requestsTo("/api/v1/tasks/completed/by_completion_date"), "content window only")
	require.Equal(t, 2, f.requestsTo("/api/v1/user"), "one account check per session")
	require.Empty(t, f.posts)
}

// Contract: an empty completion-history reply fails the run, so the saved sync
// point cannot move past completions that were never imported.
func TestRunnerEmptyHistoryReplyKeepsSyncPoint(t *testing.T) {
	ctx := t.Context()
	store, binding, f := newRunnerFixture(t, "one-way")
	_, err := f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	binding, err = store.IssueSyncBindingByProject(ctx, binding.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, binding.LastCursorAt)
	saved := *binding.LastCursorAt
	f.completeInTodoist()
	f.mu.Lock()
	f.empty = map[string]bool{"/api/v1/tasks/completed/by_completion_date": true}
	f.mu.Unlock()
	_, err = f.runner(store).RunOnce(ctx, binding.ID)
	require.Error(t, err)
	binding, err = store.IssueSyncBindingByProject(ctx, binding.ProjectID)
	require.NoError(t, err)
	require.True(t, binding.LastCursorAt.Equal(saved))
}

// Contract: when Todoist reports a null update time, a verified Kata reopen
// still clears its pending intent, so a later Todoist completion stands.
func TestRunnerReopenWithUnknownUpdateTimeClearsPendingIntent(t *testing.T) {
	ctx := t.Context()
	store, binding, f := newRunnerFixture(t, "two-way")
	f.nullFields = []string{"updated_at"}
	_, err := f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	f.completeInTodoist()
	_, err = f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	mapping, err := store.ImportMappingBySource(ctx, binding.ProjectID, binding.SourceKey, "issue", "task:"+f.row.ID)
	require.NoError(t, err)
	_, _, changed, err := store.ReopenIssue(ctx, *mapping.IssueID, "worker")
	require.NoError(t, err)
	require.True(t, changed)
	_, err = f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Len(t, f.posts, 1)
	pending, err := store.CountPendingIssueStatuses(ctx, binding.ID)
	require.NoError(t, err)
	require.Zero(t, pending)
	f.completeInTodoist()
	_, err = f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Len(t, f.posts, 1, "a completed task must not be reopened again")
}

// Contract: an archived Todoist project receives no status writes, even though
// status delivery runs before the content import that also rejects it.
func TestRunnerArchivedProjectSendsNoStatusWrite(t *testing.T) {
	ctx := t.Context()
	store, binding, f := newRunnerFixture(t, "two-way")
	_, err := f.runner(store).RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	mapping, err := store.ImportMappingBySource(ctx, binding.ProjectID, binding.SourceKey, "issue", "task:"+f.row.ID)
	require.NoError(t, err)
	_, _, changed, err := store.CloseIssueWithEvents(ctx, *mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	require.True(t, changed)
	f.mu.Lock()
	f.projectArchived = true
	f.mu.Unlock()
	_, err = f.runner(store).RunOnce(ctx, binding.ID)
	require.Error(t, err)
	require.Empty(t, f.posts)
}
