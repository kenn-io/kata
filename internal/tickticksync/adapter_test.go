package tickticksync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/issuesync"
)

type sourceFixture struct {
	data    ProjectData
	missing map[string]Task
	reads   []string
}

func (f *sourceFixture) ForRun(context.Context, Config) (Session, error) { return f, nil }
func (f *sourceFixture) Project(context.Context) (Project, error)        { return f.data.Project, nil }
func (f *sourceFixture) Data(context.Context) (ProjectData, error)       { return f.data, nil }
func (f *sourceFixture) Task(_ context.Context, id string) (Task, error) {
	f.reads = append(f.reads, id)
	t, ok := f.missing[id]
	if !ok {
		return Task{}, &issuesync.StatusError{HTTPStatus: 404, Message: "not found", Blocked: true}
	}
	if t.ProjectID != f.data.Project.ID {
		return Task{}, errTaskOutsideProject
	}
	return t, nil
}
func sourceData() *sourceFixture {
	return &sourceFixture{data: ProjectData{Project: Project{ID: "project-1", Kind: "TASK", Name: "Example tasks"}, Tasks: []Task{{ID: "task-1", ProjectID: "project-1", Title: "Task", Status: new(0)}}}, missing: map[string]Task{}}
}
func adapterDB(t *testing.T) (*sqlitestore.Store, db.IssueSyncBinding) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	s, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	p, err := s.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	raw, err := EncodeConfig(Config{ProjectID: "project-1", TitlePrefix: new(false)})
	require.NoError(t, err)
	b, err := s.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "ticktick", SourceKey: "ticktick:project-1", RemoteID: "project-1", DisplayName: "Example tasks", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return s, b
}

type missingTaskStore struct {
	db.Storage
	mappings []db.ImportMapping
	issues   []db.Issue
}

func (s *missingTaskStore) ImportMappingsByProjectSource(context.Context, int64, string) ([]db.ImportMapping, error) {
	return s.mappings, nil
}

func (s *missingTaskStore) ListIssues(context.Context, db.ListIssuesParams) ([]db.Issue, error) {
	return s.issues, nil
}

func TestOpenMissingTasksReturnsNonNilEmptyResult(t *testing.T) {
	adapter := NewAdapter(&missingTaskStore{}, nil)
	got, err := adapter.openMissingTasks(context.Background(), db.IssueSyncBinding{ProjectID: 1, SourceKey: "ticktick:project-1"}, map[string]bool{})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestOpenMissingTasksRejectsUnmappedIssueResult(t *testing.T) {
	mappedIssueID := int64(1)
	store := &missingTaskStore{
		mappings: []db.ImportMapping{{ObjectType: "issue", ExternalID: "task:task-1", IssueID: &mappedIssueID}},
		issues:   []db.Issue{{ID: 2, Status: "open"}},
	}
	adapter := NewAdapter(store, nil)
	got, err := adapter.openMissingTasks(context.Background(), db.IssueSyncBinding{ProjectID: 1, SourceKey: "ticktick:project-1"}, map[string]bool{})
	require.ErrorContains(t, err, "no task mapping")
	require.Nil(t, got)
}

func mappedIssue(t *testing.T, s db.Storage, b db.IssueSyncBinding, id string) db.Issue {
	t.Helper()
	m, err := s.ImportMappingBySource(context.Background(), b.ProjectID, b.SourceKey, "issue", "task:"+id)
	require.NoError(t, err)
	i, err := s.IssueByID(context.Background(), *m.IssueID)
	require.NoError(t, err)
	return i
}

func TestAdapterReplayAndMissingTaskRecovery(t *testing.T) {
	ctx := context.Background()
	s, b := adapterDB(t)
	f := sourceData()
	at := time.Now().UTC().Add(-time.Minute)
	r := NewRunner(RunnerConfig{Store: s, Fetcher: f, Clock: func() time.Time { return at }})
	res, err := r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, res.Import.Created)
	first := mappedIssue(t, s, b, "task-1")
	at = at.Add(time.Second)
	res, err = r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, res.Import.Unchanged)
	require.Equal(t, first.Revision, mappedIssue(t, s, b, "task-1").Revision)
	f.data.Tasks = nil
	f.missing["task-1"] = Task{ID: "task-1", ProjectID: "project-1", Title: "Task", Status: new(2)}
	at = at.Add(time.Second)
	_, err = r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", mappedIssue(t, s, b, "task-1").Status)
	delete(f.missing, "task-1")
	at = at.Add(time.Second)
	_, err = r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", mappedIssue(t, s, b, "task-1").Status)
	f.data.Project.Closed = true
	at = at.Add(time.Second)
	_, err = r.RunOnce(ctx, b.ID)
	require.Error(t, err)
	require.Equal(t, "closed", mappedIssue(t, s, b, "task-1").Status)
}

func TestPrefixedContentUpdateKeepsTimestampForOneWayCompletion(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	raw, err := EncodeConfig(Config{ProjectID: "project-1", StatusSync: "two-way", TitlePrefix: new(true)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)

	remoteTitle, remoteStatus := "Task", 0
	projectBody := `{"id":"project-1","kind":"TASK","permission":"write"}`
	client, clientNow := testClient(t, func(req *http.Request) (*http.Response, error) {
		task := fmt.Sprintf(`{"id":"task-1","projectId":"project-1","title":%q,"status":%d}`, remoteTitle, remoteStatus)
		switch req.URL.Path {
		case "/open/v1/project/project-1/data":
			return reply(http.StatusOK, `{"project":`+projectBody+`,"tasks":[`+task+`]}`), nil
		case "/open/v1/project/project-1/task/task-1":
			return reply(http.StatusOK, task), nil
		case "/open/v1/project/project-1":
			return reply(http.StatusOK, projectBody), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
	})
	at := time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)
	*clientNow = at
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: client, Clock: func() time.Time { return at }})
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)

	remoteTitle = "Updated source title"
	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	issue := mappedIssue(t, store, b, "task-1")
	require.Equal(t, "[TickTick] Updated source title", issue.Title)
	mapping, err := store.ImportMappingBySource(ctx, b.ProjectID, b.SourceKey, "issue", "task:task-1")
	require.NoError(t, err)
	require.NotNil(t, mapping.SourceUpdatedAt)
	require.Equal(t, at, *mapping.SourceUpdatedAt)

	remoteStatus = 2
	raw, err = EncodeConfig(Config{ProjectID: "project-1", StatusSync: "one-way", TitlePrefix: new(true)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", mappedIssue(t, store, b, "task-1").Status)
}

func TestOneWayStatusChangeFromTwoWayPreservesNewerLocalEdit(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	wrapped := &failOnceImportStore{Store: store}
	raw, err := EncodeConfig(Config{ProjectID: "project-1", StatusSync: "two-way", TitlePrefix: new(false)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)

	remoteStatus := 0
	projectBody := `{"id":"project-1","kind":"TASK","permission":"write"}`
	client, clientNow := testClient(t, func(req *http.Request) (*http.Response, error) {
		task := fmt.Sprintf(`{"id":"task-1","projectId":"project-1","title":"Source task","status":%d}`, remoteStatus)
		switch req.URL.Path {
		case "/open/v1/project/project-1/data":
			return reply(http.StatusOK, `{"project":`+projectBody+`,"tasks":[`+task+`]}`), nil
		case "/open/v1/project/project-1/task/task-1":
			return reply(http.StatusOK, task), nil
		case "/open/v1/project/project-1":
			return reply(http.StatusOK, projectBody), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
	})
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	*clientNow = at
	runner := NewRunner(RunnerConfig{Store: wrapped, Fetcher: client, Clock: func() time.Time { return at }})
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)

	issue := mappedIssue(t, store, b, "task-1")
	claim, err := store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: "worker", TTL: 2 * time.Minute, Now: at})
	require.NoError(t, err)
	require.NotNil(t, claim.Issue.AssignmentExpiresOn)
	assignmentExpiry := *claim.Issue.AssignmentExpiresOn
	localTitle := "Newer local title"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &localTitle, Actor: "worker"})
	require.NoError(t, err)
	require.True(t, mappedIssue(t, store, b, "task-1").UpdatedAt.After(issue.UpdatedAt))

	remoteStatus = 2
	raw, err = EncodeConfig(Config{ProjectID: "project-1", StatusSync: "one-way", TitlePrefix: new(false)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	at = time.Now().UTC().Truncate(time.Millisecond).Add(time.Minute)
	*clientNow = at
	wrapped.failNext = true
	_, err = runner.RunOnce(ctx, b.ID)
	require.ErrorContains(t, err, "injected final content import failure")
	issue = mappedIssue(t, store, b, "task-1")
	require.Equal(t, "open", issue.Status)
	require.Equal(t, localTitle, issue.Title)

	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)

	issue = mappedIssue(t, store, b, "task-1")
	require.Equal(t, "closed", issue.Status)
	require.Equal(t, localTitle, issue.Title, "a status-only source change must preserve newer local content")
	require.Equal(t, new("worker"), issue.Owner)
	require.Nil(t, issue.AssignmentExpiresOn, "closing on a two-way to one-way transition keeps the owner permanently")
	expiryEvents, err := store.ExpireAssignments(ctx, db.ExpireAssignmentsParams{ProjectID: b.ProjectID, Now: assignmentExpiry.Add(time.Minute)})
	require.NoError(t, err)
	require.Empty(t, expiryEvents, "the expired assignment must not clear the closed issue owner")
	require.Equal(t, new("worker"), mappedIssue(t, store, b, "task-1").Owner)
}

func TestOneWaySyncRetiresLocallyClosedMissingTaskCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	fixture := sourceData()
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: fixture, Clock: func() time.Time { return at }})
	_, err := runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	issue := mappedIssue(t, store, b, "task-1")
	_, _, changed, err := store.CloseIssue(ctx, issue.ID, "done", "worker", "", nil)
	require.NoError(t, err)
	require.True(t, changed)

	fixture.data.Tasks = nil
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	saved, err := store.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	checkpoint, err := DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	require.NotContains(t, checkpoint.Versions, "task-1", "one-way mode has no missing-task status observation to support recovery")
}

func TestOneWayStatusBaselineSurvivesReappearanceAndImportFailure(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	wrapped := &failOnceImportStore{Store: store}
	fixture := sourceData()
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	runner := NewRunner(RunnerConfig{Store: wrapped, Fetcher: fixture, Clock: func() time.Time { return at }})

	_, err := runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)

	fixture.data.Tasks[0].Status = new(2)
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", mappedIssue(t, store, b, "task-1").Status)

	fixture.data.Tasks = nil
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	saved, err := store.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	checkpoint, err := DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	require.NotContains(t, checkpoint.Versions, "task-1")

	fixture.data.Tasks = []Task{{ID: "task-1", ProjectID: "project-1", Title: "Task", Status: new(0)}}
	at = at.Add(time.Minute)
	wrapped.failNext = true
	_, err = runner.RunOnce(ctx, b.ID)
	require.ErrorContains(t, err, "injected final content import failure")
	saved, err = store.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	checkpoint, err = DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	require.True(t, checkpoint.Versions["task-1"].PendingStatus, "the reappearing status baseline must stay pending until its import commits")

	at = at.Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "open", mappedIssue(t, store, b, "task-1").Status)

	issue := mappedIssue(t, store, b, "task-1")
	localTitle, localBody := "Local title after reappearance", "Local body after reappearance"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &localTitle, Body: &localBody, Actor: "worker"})
	require.NoError(t, err)

	fixture.data.Tasks[0].Status = new(2)
	at = time.Now().UTC().Truncate(time.Millisecond).Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	issue = mappedIssue(t, store, b, "task-1")
	require.Equal(t, "closed", issue.Status, "the completion after the reopened baseline must not replay the older completion acknowledgement")
	require.Equal(t, localTitle, issue.Title)
	require.Equal(t, localBody, issue.Body)
}

func TestTwoWayRetiresClosedMissingTaskAfterTerminalReadback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		moved    bool
		readback *Task
	}{{name: "deleted"},
		{name: "moved", moved: true},
		{name: "abandoned", readback: &Task{ID: "task-1", ProjectID: "project-1", Title: "Abandoned task", Status: new(-1)}},
		{name: "note", readback: &Task{ID: "task-1", ProjectID: "project-1", Title: "Note task", Kind: "NOTE", Status: new(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, b := adapterDB(t)
			fixture := sourceData()
			at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
			_, err := NewRunner(RunnerConfig{Store: store, Fetcher: fixture, Clock: func() time.Time { return at }}).RunOnce(ctx, b.ID)
			require.NoError(t, err)

			raw, err := EncodeConfig(Config{ProjectID: "project-1", StatusSync: "two-way", TitlePrefix: new(false)})
			require.NoError(t, err)
			b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
			require.NoError(t, err)
			issue := mappedIssue(t, store, b, "task-1")
			_, events, changed, err := store.CloseIssueWithEvents(ctx, issue.ID, "done", "worker", "Completed task", nil)
			require.NoError(t, err)
			require.True(t, changed)
			require.Len(t, events, 1)

			started := at.Add(time.Hour)
			claimed, ok, err := store.ClaimIssueSyncBinding(ctx, b.ID, "ticktick", started, started.Add(-30*time.Minute))
			require.NoError(t, err)
			require.True(t, ok)
			mapping, err := store.ImportMappingBySource(ctx, b.ProjectID, b.SourceKey, "issue", "task:task-1")
			require.NoError(t, err)
			openStatus := "0"
			_, _, err = store.ObserveIssueStatus(ctx, db.IssueStatusObservationParams{
				Guard:     db.IssueSyncImportGuard{BindingID: b.ID, Provider: b.Provider, StartedAt: started},
				MappingID: mapping.ID, ExternalID: mapping.ExternalID, IssueUID: issue.UID,
				Observation: db.IssueStatusObservation{Raw: &openStatus, Version: started}, Status: "open",
			})
			require.NoError(t, err)

			fixture.data.Tasks = nil
			if tc.moved {
				fixture.missing["task-1"] = Task{ID: "task-1", ProjectID: "other-project", Title: "Moved task", Status: new(0)}
			} else if tc.readback != nil {
				fixture.missing["task-1"] = *tc.readback
			}
			prepared, err := NewAdapter(store, fixture).Prepare(ctx, claimed, started)
			require.NoError(t, err)
			require.Equal(t, []string{"task-1"}, fixture.reads, "a bounded task lookup must establish whether a closed missing candidate still exists")
			checkpoint, err := DecodeCheckpoint(prepared.Binding.Config)
			require.NoError(t, err)
			require.NotContains(t, checkpoint.Versions, "task-1", "a terminal bounded readback retires the stale content fingerprint")
			require.Equal(t, "closed", mappedIssue(t, store, b, "task-1").Status, "retiring a source fingerprint must preserve the local issue status")

			current, err := store.IssueStatusMappingByID(ctx, db.IssueSyncImportGuard{
				BindingID: b.ID, Provider: b.Provider, StartedAt: started, BindingUpdatedAt: &prepared.Binding.UpdatedAt,
			}, mapping.ID)
			require.NoError(t, err)
			require.Equal(t, events[0].UID, current.State.PendingEventUID, "retiring the content checkpoint must preserve the independent pending status intent")
		})
	}
}

func TestTwoWayCompletedMissingTaskRecoverySurvivesStatusPageAdvance(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	wrapped := &failOnceImportStore{Store: store}
	raw, err := EncodeConfig(Config{ProjectID: "project-1", StatusSync: "two-way", TitlePrefix: new(false)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)

	const fillerCount = 101
	ids := make([]string, 0, fillerCount+1)
	for i := range fillerCount {
		ids = append(ids, fmt.Sprintf("b-%03d", i))
	}
	ids = append(ids, "a-target") // Imported last, but included in the first bounded content lookup.
	visible := true
	statuses := make(map[string]int, len(ids))
	titles := make(map[string]string, len(ids))
	contents := make(map[string]string, len(ids))
	for _, id := range ids {
		statuses[id] = 0
		titles[id] = "Initial " + id
		contents[id] = "Initial body"
	}
	targetReads := 0
	projectBody := `{"id":"project-1","name":"Example tasks","kind":"TASK","permission":"write"}`
	taskJSON := func(id string) string {
		return fmt.Sprintf(`{"id":%q,"projectId":"project-1","title":%q,"content":%q,"status":%d}`, id, titles[id], contents[id], statuses[id])
	}
	client, clientNow := testClient(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Path == "/open/v1/project/project-1/data":
			if !visible {
				return reply(http.StatusOK, `{"project":`+projectBody+`,"tasks":[]}`), nil
			}
			items := make([]string, 0, len(ids))
			for _, id := range ids {
				items = append(items, taskJSON(id))
			}
			return reply(http.StatusOK, `{"project":`+projectBody+`,"tasks":[`+strings.Join(items, ",")+`]}`), nil
		case req.URL.Path == "/open/v1/project/project-1":
			return reply(http.StatusOK, projectBody), nil
		case strings.HasPrefix(req.URL.Path, "/open/v1/project/project-1/task/"):
			id := strings.TrimPrefix(req.URL.Path, "/open/v1/project/project-1/task/")
			if id == "a-target" {
				targetReads++
			}
			return reply(http.StatusOK, taskJSON(id)), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
	})
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	*clientNow = at
	runner := issuesync.NewRunner(issuesync.RunnerConfig{Store: wrapped, Adapter: NewAdapter(wrapped, client), Clock: func() time.Time { return at }})
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "Initial a-target", mappedIssue(t, store, b, "a-target").Title)

	visible = false
	statuses["a-target"] = 2
	titles["a-target"] = "Final target title"
	contents["a-target"] = "Final target body"
	at = at.Add(time.Minute)
	*clientNow = at
	wrapped.failNext = true
	_, err = runner.RunOnce(ctx, b.ID)
	require.ErrorContains(t, err, "injected final content import failure")
	require.Equal(t, 1, targetReads, "the target is in the content lookup but beyond the 100-row status page")
	saved, err := store.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	checkpoint, err := DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	wasRecoveryPending := checkpoint.Versions["a-target"].PendingRecovery

	// The next status page reaches the target and closes its local issue. Content
	// recovery then needs its pending marker to survive the status transition.
	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", mappedIssue(t, store, b, "a-target").Status)

	// The content cursor wraps on the following run and retries the completed
	// task after its local issue has closed.
	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	issue := mappedIssue(t, store, b, "a-target")
	require.Equal(t, "Final target title", issue.Title)
	require.Contains(t, issue.Body, "Final target body")
	require.True(t, wasRecoveryPending, "the failed completed-task import must stage recovery regardless of local status")
}

func TestOneWayStatusRetryDoesNotUndoLocalReopenAfterFinalizeFailure(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	wrapped := &failOnceFinalizeStore{Store: store}
	raw, err := EncodeConfig(Config{ProjectID: "project-1", StatusSync: "two-way", TitlePrefix: new(false)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)

	remoteStatus := 0
	projectBody := `{"id":"project-1","name":"Example tasks","kind":"TASK","permission":"write"}`
	client, clientNow := testClient(t, func(req *http.Request) (*http.Response, error) {
		task := func(id string) string {
			return fmt.Sprintf(`{"id":%q,"projectId":"project-1","title":"Source task","status":%d}`, id, remoteStatus)
		}
		switch req.URL.Path {
		case "/open/v1/project/project-1/data":
			return reply(http.StatusOK, `{"project":`+projectBody+`,"tasks":[`+task("task-1")+`,`+task("task-2")+`]}`), nil
		case "/open/v1/project/project-1/task/task-1":
			return reply(http.StatusOK, task("task-1")), nil
		case "/open/v1/project/project-1/task/task-2":
			return reply(http.StatusOK, task("task-2")), nil
		case "/open/v1/project/project-1":
			return reply(http.StatusOK, projectBody), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
	})
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	*clientNow = at
	runner := issuesync.NewRunner(issuesync.RunnerConfig{Store: wrapped, Adapter: NewAdapter(wrapped, client), Clock: func() time.Time { return at }, InitialBatchSize: 1})
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)

	remoteStatus = 2
	raw, err = EncodeConfig(Config{ProjectID: "project-1", StatusSync: "one-way", TitlePrefix: new(false)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	at = at.Add(time.Minute)
	*clientNow = at
	wrapped.failOnRefresh = 2
	_, err = runner.RunOnce(ctx, b.ID)
	require.ErrorContains(t, err, "injected checkpoint finalization failure")
	issue := mappedIssue(t, store, b, "task-1")
	require.Equal(t, "closed", issue.Status, "the status-only import committed before finalization failed")
	require.Equal(t, "closed", mappedIssue(t, store, b, "task-2").Status)
	_, _, changed, err := store.ReopenIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	require.True(t, changed)

	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "open", mappedIssue(t, store, b, "task-1").Status, "retrying an acknowledged completion must preserve a later local reopen")
	require.Equal(t, "closed", mappedIssue(t, store, b, "task-2").Status)
}

func TestTwoWayRecoveryKeepsContentCheckpointAcrossFinalizeFailure(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	wrapped := &failOnceFinalizeStore{Store: store}
	raw, err := EncodeConfig(Config{ProjectID: "project-1", StatusSync: "two-way", TitlePrefix: new(false)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)

	visible := true
	remoteTitle, remoteContent, remoteStatus := "Initial title", "Initial body", 0
	taskReads := 0
	projectBody := `{"id":"project-1","kind":"TASK","permission":"write"}`
	client, clientNow := testClient(t, func(req *http.Request) (*http.Response, error) {
		task := fmt.Sprintf(`{"id":"task-1","projectId":"project-1","title":%q,"content":%q,"status":%d}`, remoteTitle, remoteContent, remoteStatus)
		switch req.URL.Path {
		case "/open/v1/project/project-1/data":
			items := "[]"
			if visible {
				items = "[" + task + "]"
			}
			return reply(http.StatusOK, `{"project":`+projectBody+`,"tasks":`+items+`}`), nil
		case "/open/v1/project/project-1/task/task-1":
			taskReads++
			return reply(http.StatusOK, task), nil
		case "/open/v1/project/project-1":
			return reply(http.StatusOK, projectBody), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
	})
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	*clientNow = at
	runner := NewRunner(RunnerConfig{Store: wrapped, Fetcher: client, Clock: func() time.Time { return at }})
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)

	visible = false
	remoteTitle, remoteContent, remoteStatus = "Final title", "Final body", 2
	at = at.Add(time.Minute)
	*clientNow = at
	wrapped.failOnRefresh = 2 // persist the pending observation, then fail finalization after import
	_, err = runner.RunOnce(ctx, b.ID)
	require.ErrorContains(t, err, "injected checkpoint finalization failure")
	issue := mappedIssue(t, store, b, "task-1")
	require.Equal(t, "closed", issue.Status)
	require.Equal(t, "Final title", issue.Title, "recovered source content must import before finalization")
	require.Contains(t, issue.Body, "Final body")
	saved, err := store.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	checkpoint, err := DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	version, ok := checkpoint.Versions["task-1"]
	require.True(t, ok)
	require.True(t, version.PendingRecovery, "the durable checkpoint must retain an unfinished recovery marker")
	wantContent, err := taskContentHash(Task{ID: "task-1", ProjectID: "project-1", Title: remoteTitle, Content: remoteContent, Status: new(remoteStatus)})
	require.NoError(t, err)
	require.Equal(t, wantContent, version.Hash, "the recovery fingerprint must be persisted before importing the item")
	require.Equal(t, at, version.Version)

	localTitle, localBody := "Local edit after recovery", "Local body after recovery"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &localTitle, Body: &localBody, Actor: "worker"})
	require.NoError(t, err)

	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	issue = mappedIssue(t, store, b, "task-1")
	require.Equal(t, localTitle, issue.Title, "retrying an unchanged recovery must not overwrite a newer local edit")
	require.Equal(t, localBody, issue.Body)

	saved, err = store.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	checkpoint, err = DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	version, ok = checkpoint.Versions["task-1"]
	require.True(t, ok, "successful recovery remains until the next collection confirms retirement")
	require.False(t, version.PendingRecovery, "successful finalization clears the recovery marker")

	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	saved, err = store.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	checkpoint, err = DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	require.NotContains(t, checkpoint.Versions, "task-1", "completed recovery retires after finalization succeeds")
	require.GreaterOrEqual(t, taskReads, 4, "status and content reads must retry recovery after finalization fails")
}

func TestTwoWayCompletionImportsFinalContentWhenTaskDisappearsBetweenPolls(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	wrapped := &failOnceImportStore{Store: store}
	raw, err := EncodeConfig(Config{ProjectID: "project-1", StatusSync: "two-way", TitlePrefix: new(false)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)

	visible := true
	remoteTitle, remoteContent, remoteStatus := "Initial title", "Initial body", 0
	taskReads := 0
	projectBody := `{"id":"project-1","kind":"TASK","permission":"write"}`
	client, clientNow := testClient(t, func(req *http.Request) (*http.Response, error) {
		task := fmt.Sprintf(`{"id":"task-1","projectId":"project-1","title":%q,"content":%q,"status":%d}`, remoteTitle, remoteContent, remoteStatus)
		switch req.URL.Path {
		case "/open/v1/project/project-1/data":
			items := "[]"
			if visible {
				items = "[" + task + "]"
			}
			return reply(http.StatusOK, `{"project":`+projectBody+`,"tasks":`+items+`}`), nil
		case "/open/v1/project/project-1/task/task-1":
			taskReads++
			return reply(http.StatusOK, task), nil
		case "/open/v1/project/project-1":
			return reply(http.StatusOK, projectBody), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
	})
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	*clientNow = at
	runner := NewRunner(RunnerConfig{Store: wrapped, Fetcher: client, Clock: func() time.Time { return at }})
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)

	visible = false
	remoteTitle, remoteContent, remoteStatus = "Final title", "Final body", 2
	at = at.Add(time.Minute)
	*clientNow = at
	wrapped.failNext = true
	_, err = runner.RunOnce(ctx, b.ID)
	require.ErrorContains(t, err, "injected final content import failure")
	issue := mappedIssue(t, store, b, "task-1")
	require.Equal(t, "closed", issue.Status)
	require.Equal(t, "Initial title", issue.Title)

	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)

	issue = mappedIssue(t, store, b, "task-1")
	require.Equal(t, "closed", issue.Status)
	require.Equal(t, "Final title", issue.Title)
	require.Contains(t, issue.Body, "Final body")
	require.Equal(t, 4, taskReads, "the failed import must retry final content after the status read")

	at = at.Add(time.Minute)
	*clientNow = at
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 5, taskReads, "only the status pass reads the task after final content succeeds")
	saved, err := store.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	checkpoint, err := DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	require.NotContains(t, checkpoint.Versions, "task-1", "completed content recovery is retired on the next poll")
}

func TestReenablingTitlePrefixRemovesSourceManagedLabel(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	f := sourceData()
	at := time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: f, Clock: func() time.Time { return at }})
	_, err := runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	issue := mappedIssue(t, store, b, "task-1")
	hasLabel, err := store.HasLabel(ctx, issue.ID, "ticktick")
	require.NoError(t, err)
	require.True(t, hasLabel)

	raw, err := EncodeConfig(Config{ProjectID: "project-1", StatusSync: "one-way", TitlePrefix: new(true)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	at = at.Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)

	issue = mappedIssue(t, store, b, "task-1")
	require.Equal(t, "[TickTick] Task", issue.Title)
	hasLabel, err = store.HasLabel(ctx, issue.ID, "ticktick")
	require.NoError(t, err)
	require.False(t, hasLabel)
}

type failOnceImportStore struct {
	*sqlitestore.Store
	failNext bool
}

func (s *failOnceImportStore) ImportBatch(ctx context.Context, p db.ImportBatchParams) (db.ImportBatchResult, []db.Event, error) {
	if s.failNext {
		s.failNext = false
		return db.ImportBatchResult{}, nil, errors.New("injected final content import failure")
	}
	return s.Store.ImportBatch(ctx, p)
}

type failOnceFinalizeStore struct {
	*sqlitestore.Store
	failOnRefresh int
	refreshCalls  int
}

func (s *failOnceFinalizeStore) RefreshIssueSyncBinding(ctx context.Context, p db.IssueSyncBindingUpdateParams) (db.IssueSyncBinding, error) {
	if s.failOnRefresh > 0 {
		s.refreshCalls++
		if s.refreshCalls == s.failOnRefresh {
			s.failOnRefresh = 0
			return db.IssueSyncBinding{}, errors.New("injected checkpoint finalization failure")
		}
	}
	return s.Store.RefreshIssueSyncBinding(ctx, p)
}

type failingImportStore struct {
	db.Storage
	calls int
	fail  bool
}

func (s *failingImportStore) ImportBatch(ctx context.Context, p db.ImportBatchParams) (db.ImportBatchResult, []db.Event, error) {
	s.calls++
	if s.fail && s.calls == 2 {
		return db.ImportBatchResult{}, nil, errors.New("injected second chunk failure")
	}
	return s.Storage.ImportBatch(ctx, p)
}
func TestAdapterStagedVersionsSurvivePartialImport(t *testing.T) {
	ctx := context.Background()
	s, b := adapterDB(t)
	f := sourceData()
	f.data.Tasks = append(f.data.Tasks, Task{ID: "task-2", ProjectID: "project-1", Title: "Second", Status: new(0)})
	at := time.Now().UTC().Add(-time.Minute)
	wrapped := &failingImportStore{Storage: s, fail: true}
	r := issuesync.NewRunner(issuesync.RunnerConfig{Store: wrapped, Adapter: NewAdapter(wrapped, f), Clock: func() time.Time { return at }, InitialBatchSize: 1})
	_, err := r.RunOnce(ctx, b.ID)
	require.ErrorContains(t, err, "second chunk")
	i := mappedIssue(t, s, b, "task-1")
	local := "Local edit between chunks"
	_, _, _, err = s.EditIssue(ctx, db.EditIssueParams{IssueID: i.ID, Title: &local, Actor: "worker"})
	require.NoError(t, err)
	wrapped.fail = false
	at = time.Now().UTC().Add(time.Minute)
	_, err = r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, local, mappedIssue(t, s, b, "task-1").Title)
	require.Equal(t, "Second", mappedIssue(t, s, b, "task-2").Title)
	saved, err := s.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	cp, err := DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	require.Len(t, cp.Versions, 2)
	_, err = s.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{BindingID: b.ID, DisplayName: b.DisplayName, Config: saved.Config, ReplaceProviderCheckpoint: true})
	require.Error(t, err, "unclaimed checkpoint replacement must be rejected")
}

func TestAdapterMissingLookupIsBoundedAndResumes(t *testing.T) {
	ctx := context.Background()
	s, b := adapterDB(t)
	f := sourceData()
	at := time.Now().UTC().Add(-time.Minute)
	for n := range 205 {
		f.data.Tasks = append(f.data.Tasks, Task{ID: fmt.Sprintf("old-%03d", n), ProjectID: "project-1", Title: "Historical", Status: new(0)})
	}
	r := NewRunner(RunnerConfig{Store: s, Fetcher: f, Clock: func() time.Time { return at }})
	_, err := r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	f.data.Tasks = f.data.Tasks[:1]
	seen := map[string]bool{}
	for range 3 {
		f.reads = nil
		at = at.Add(time.Second)
		res, err := r.RunOnce(ctx, b.ID)
		require.NoError(t, err)
		require.LessOrEqual(t, len(f.reads), 100)
		require.Equal(t, 1, res.Import.Unchanged)
		for _, id := range f.reads {
			seen[id] = true
		}
	}
	require.Len(t, seen, 205)
}

func TestAdapterTwoWayCompletionRetainsReopenIntent(t *testing.T) {
	ctx := context.Background()
	store, b := adapterDB(t)
	raw, err := EncodeConfig(Config{ProjectID: "project-1", StatusSync: "two-way", TitlePrefix: new(false)})
	require.NoError(t, err)
	b, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	remote := 0
	posts := 0
	projectBody := `{"id":"project-1","kind":"TASK","permission":"write"}`
	client, now := testClient(t, func(req *http.Request) (*http.Response, error) {
		if req.Method == "POST" {
			posts++
			remote = 2
			return reply(200, ""), nil
		}
		task := fmt.Sprintf(`{"id":"task-1","projectId":"project-1","title":"Task","status":%d}`, remote)
		if strings.HasSuffix(req.URL.Path, "/data") {
			return reply(200, `{"project":`+projectBody+`,"tasks":[`+task+`]}`), nil
		}
		if strings.HasSuffix(req.URL.Path, "/task/task-1") {
			return reply(200, task), nil
		}
		return reply(200, projectBody), nil
	})
	at := time.Now().UTC().Add(-time.Minute)
	*now = at
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: client, Clock: func() time.Time { return *now }})
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	issue := mappedIssue(t, store, b, "task-1")
	_, _, _, err = store.CloseIssueWithEvents(ctx, issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	*now = time.Now().UTC().Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 2, remote)
	require.Equal(t, 1, posts)
	*now = now.Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, 1, posts, "remote observations must not echo completion")
	_, _, _, err = store.ReopenIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	*now = now.Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.True(t, issuesync.IsBlockedStatusWarning(err))
	require.Equal(t, 1, posts)
	require.Equal(t, "open", mappedIssue(t, store, b, "task-1").Status)
	var pending *string
	require.NoError(t, store.QueryRowContext(ctx, `SELECT pending_event_uid FROM import_mappings WHERE issue_id=?`, issue.ID).Scan(&pending))
	require.NotNil(t, pending)
	remote = 0
	*now = now.Add(time.Minute)
	_, err = runner.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.NoError(t, store.QueryRowContext(ctx, `SELECT pending_event_uid FROM import_mappings WHERE issue_id=?`, issue.ID).Scan(&pending))
	require.Nil(t, pending)
	require.Equal(t, 1, posts)
}

// Duplicate collection rows do not consume the unique-task allowance needed
// to recover a previously mapped task missing from the current collection.
func TestAdapterRecoversMissingAtUniqueTaskLimit(t *testing.T) {
	ctx := context.Background()
	s, b := adapterDB(t)
	f := sourceData()
	at := time.Now().UTC().Add(-time.Minute)
	_, err := NewRunner(RunnerConfig{Store: s, Fetcher: f, Clock: func() time.Time { return at }}).RunOnce(ctx, b.ID)
	require.NoError(t, err)
	f.missing["task-1"] = f.data.Tasks[0]
	f.data.Tasks = nil
	for i := range maxTasks - 1 {
		f.data.Tasks = append(f.data.Tasks, Task{ID: fmt.Sprintf("active-%d", i), ProjectID: "project-1", Title: "Task", Status: new(0)})
	}
	f.data.Tasks = append(f.data.Tasks, f.data.Tasks[0])
	at = at.Add(time.Second)
	current, claimed, err := s.ClaimIssueSyncBinding(ctx, b.ID, "ticktick", at, at.Add(-30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	prepared, err := NewAdapter(s, f).Prepare(ctx, current, at)
	require.NoError(t, err)
	require.Len(t, prepared.Batch.Items, maxTasks)
	require.Equal(t, []string{"task-1"}, f.reads)
	cp, err := DecodeCheckpoint(prepared.Binding.Config)
	require.NoError(t, err)
	require.Len(t, cp.Versions, maxTasks)
	// A new unique task still exceeds the documented durable tracking limit.
	f.data.Tasks[len(f.data.Tasks)-1] = Task{ID: "active-overflow", ProjectID: "project-1", Title: "Task", Status: new(0)}
	_, err = NewAdapter(s, f).Prepare(ctx, prepared.Binding, at)
	require.ErrorContains(t, err, "10000 tracked tasks")
	saved, err := s.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, string(prepared.Binding.Config), string(saved.Config))
}

// Completed history must not accumulate lookups or checkpoint entries: a
// missing task whose Kata issue is closed stops being read until it reappears.
func TestAdapterRetiresClosedMissingTasks(t *testing.T) {
	ctx := context.Background()
	s, b := adapterDB(t)
	f := sourceData()
	f.data.Tasks = append(f.data.Tasks, Task{ID: "task-2", ProjectID: "project-1", Title: "Second", Status: new(0)})
	at := time.Now().UTC().Add(-time.Minute)
	r := NewRunner(RunnerConfig{Store: s, Fetcher: f, Clock: func() time.Time { return at }})
	_, err := r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	f.missing["task-1"] = Task{ID: "task-1", ProjectID: "project-1", Title: "Task", Status: new(2)}
	f.data.Tasks = f.data.Tasks[1:]
	at = at.Add(time.Second)
	_, err = r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", mappedIssue(t, s, b, "task-1").Status)
	f.reads = nil
	at = at.Add(time.Second)
	res, err := r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Empty(t, f.reads, "closed missing tasks are not read again")
	cp, err := DecodeCheckpoint(res.Binding.Config)
	require.NoError(t, err)
	require.NotContains(t, cp.Versions, "task-1")
	require.Contains(t, cp.Versions, "task-2")
	f.data.Tasks = append(f.data.Tasks, Task{ID: "task-1", ProjectID: "project-1", Title: "Task", Status: new(0)})
	at = at.Add(time.Second)
	_, err = r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "open", mappedIssue(t, s, b, "task-1").Status, "a reopened task is imported again")
}

// A task moved to another TickTick project stays local and does not stop
// content sync for the rest of the selected project.
func TestAdapterSkipsMissingTaskMovedToAnotherProject(t *testing.T) {
	ctx := context.Background()
	s, b := adapterDB(t)
	moved := false
	client, now := testClient(t, func(req *http.Request) (*http.Response, error) {
		project := `{"id":"project-1","kind":"TASK"}`
		if strings.HasSuffix(req.URL.Path, "/data") {
			tasks := `{"id":"task-1","projectId":"project-1","title":"Task","status":0}`
			if moved {
				tasks = ""
			}
			return reply(200, `{"project":`+project+`,"tasks":[`+tasks+`]}`), nil
		}
		if strings.HasSuffix(req.URL.Path, "/task/task-1") {
			return reply(200, `{"id":"task-1","projectId":"other-project","title":"Task","status":0}`), nil
		}
		return reply(200, project), nil
	})
	*now = time.Now().UTC().Add(-time.Minute)
	r := NewRunner(RunnerConfig{Store: s, Fetcher: client, Clock: func() time.Time { return *now }})
	_, err := r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	moved = true
	*now = now.Add(time.Minute)
	_, err = r.RunOnce(ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "open", mappedIssue(t, s, b, "task-1").Status)
}

// One-way imports and two-way status reads record the same observed_status
// value for one TickTick state, so switching modes compares like with like.
func TestObservedStatusEncodingMatchesAcrossModes(t *testing.T) {
	ctx := context.Background()
	s, b := adapterDB(t)
	f := sourceData()
	completed := Task{ID: "task-1", ProjectID: "project-1", Title: "Task", Status: new(2), CompletedTime: "2026-10-01T09:00:00.000+0000"}
	f.data.Tasks = []Task{completed}
	at := time.Now().UTC().Add(-time.Minute)
	_, err := NewRunner(RunnerConfig{Store: s, Fetcher: f, Clock: func() time.Time { return at }}).RunOnce(ctx, b.ID)
	require.NoError(t, err)
	var imported *string
	require.NoError(t, s.QueryRowContext(ctx, `SELECT observed_status FROM import_mappings WHERE external_id='task:task-1'`).Scan(&imported))
	require.NotNil(t, imported)

	c, _ := testClient(t, func(*http.Request) (*http.Response, error) {
		return reply(200, `{"id":"task-1","projectId":"project-1","status":2,"completedTime":"2026-10-01T09:00:00.000+0000"}`), nil
	})
	session, err := c.ForRun(ctx, clientConfig())
	require.NoError(t, err)
	obs, err := session.(StatusSession).ReadStatus(ctx, "task-1")
	require.NoError(t, err)
	require.Equal(t, *imported, *obs.RawStatus)
}
