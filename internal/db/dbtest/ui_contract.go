package dbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunUISnapshotCompletenessContract verifies that an unbounded browser
// snapshot returns the complete matching authority set.
func RunUISnapshotCompletenessContract(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	store := open(t)
	uiStore, ok := store.(db.UIStore)
	require.True(t, ok, "storage backend must implement db.UIStore")
	ctx := context.Background()
	project := createCursorProject(ctx, t, store)
	const issueCount = 501
	for idx := range issueCount {
		createCursorIssue(ctx, t, store, project.ID, fmt.Sprintf("Example issue %03d", idx))
	}

	snapshot, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open"})
	require.NoError(t, err)
	require.Len(t, snapshot.Issues, issueCount)
}

// RunUISnapshotAuthorityReuseContract verifies that a store omits only the
// cached collection portion when the expected cursor matches its consistent
// read, and rebuilds complete authority after a durable mutation.
func RunUISnapshotAuthorityReuseContract(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	store := open(t)
	uiStore, ok := store.(db.UIStore)
	require.True(t, ok, "storage backend must implement db.UIStore")
	ctx := context.Background()
	project := createCursorProject(ctx, t, store)
	issue := createCursorIssue(ctx, t, store, project.ID, "Selected issue")
	_, _, err := store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "important", Actor: "user-a",
	})
	require.NoError(t, err)

	full, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open"})
	require.NoError(t, err)
	require.NotEmpty(t, full.Issues)
	cursor := full.Cursor
	reused, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", SelectedIssueUID: issue.UID, IncludeHistory: true,
		ReuseAuthorityCursor: &cursor,
	})
	require.NoError(t, err)
	require.True(t, reused.AuthorityReused)
	require.Empty(t, reused.Projects)
	require.Empty(t, reused.Issues)
	require.NotNil(t, reused.SelectedIssue)
	require.Equal(t, []string{"important"}, reused.SelectedIssue.Labels)

	created := createCursorIssue(ctx, t, store, project.ID, "New issue")
	rebuilt, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", ReuseAuthorityCursor: &cursor,
	})
	require.NoError(t, err)
	require.False(t, rebuilt.AuthorityReused)
	require.Greater(t, rebuilt.Cursor, cursor)
	require.Contains(t, uiIssueUIDs(rebuilt.Issues), created.UID)
}

// RunUISnapshotMutationCursorContract verifies that every persisted mutation
// visible to a browser snapshot advances the durable event cursor. Open must
// return a fresh store for each subtest.
func RunUISnapshotMutationCursorContract(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	t.Run("state changes advance", func(t *testing.T) {
		tests := []struct {
			name string
			run  func(context.Context, *testing.T, db.Storage)
		}{
			{name: "project create", run: cursorProjectCreate},
			{name: "project rename", run: cursorProjectRename},
			{name: "project hard delete", run: cursorProjectHardDelete},
			{name: "issue", run: cursorIssue},
			{name: "metadata", run: cursorMetadata},
			{name: "owner", run: cursorOwner},
			{name: "label", run: cursorLabel},
			{name: "comment", run: cursorComment},
			{name: "link", run: cursorLink},
			{name: "checklist", run: cursorChecklist},
			{name: "close", run: cursorClose},
			{name: "reopen", run: cursorReopen},
			{name: "delete", run: cursorDelete},
			{name: "restore", run: cursorRestore},
			{name: "recurrence", run: cursorRecurrence},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				store := open(t)
				uiStore, ok := store.(db.UIStore)
				require.True(t, ok, "storage backend must implement db.UIStore")
				ctx := context.Background()
				before, err := uiStore.UIEventCursor(ctx)
				require.NoError(t, err)
				test.run(ctx, t, store)
				after, err := uiStore.UIEventCursor(ctx)
				require.NoError(t, err)
				require.Greater(t, after, before, "persisted UI state change must advance cursor")
			})
		}
	})

	t.Run("project merge advances", func(t *testing.T) {
		store := open(t)
		uiStore, ok := store.(db.UIStore)
		require.True(t, ok, "storage backend must implement db.UIStore")
		ctx := context.Background()
		target := createCursorProject(ctx, t, store)
		source, err := store.CreateProject(ctx, "example-workspace")
		require.NoError(t, err)
		before, err := uiStore.UIEventCursor(ctx)
		require.NoError(t, err)

		_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
			SourceProjectID: source.ID,
			TargetProjectID: target.ID,
		})
		require.NoError(t, err)
		after, err := uiStore.UIEventCursor(ctx)
		require.NoError(t, err)
		require.Greater(t, after, before, "project merge must invalidate existing snapshot ETags")
	})

	t.Run("no-op and rejected writes do not advance", func(t *testing.T) {
		store := open(t)
		uiStore, ok := store.(db.UIStore)
		require.True(t, ok, "storage backend must implement db.UIStore")
		ctx := context.Background()
		project := createCursorProject(ctx, t, store)
		issue := createCursorIssue(ctx, t, store, project.ID, "Unchanged title")

		before, err := uiStore.UIEventCursor(ctx)
		require.NoError(t, err)
		_, err = store.RenameProject(ctx, project.ID, project.Name)
		require.NoError(t, err)
		requireCursorUnchanged(ctx, t, uiStore, before)

		title := issue.Title
		_, event, changed, err := store.EditIssue(ctx, db.EditIssueParams{
			IssueID: issue.ID, Title: &title, Actor: "user-a",
		})
		require.NoError(t, err)
		require.False(t, changed)
		require.Nil(t, event)
		requireCursorUnchanged(ctx, t, uiStore, before)

		_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Actor: "user-a"})
		require.Error(t, err)
		requireCursorUnchanged(ctx, t, uiStore, before)

		out, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID: issue.ID, Actor: "user-a", Patch: map[string]json.RawMessage{},
		})
		require.NoError(t, err)
		require.False(t, out.Changed)
		requireCursorUnchanged(ctx, t, uiStore, before)

		_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID: issue.ID, Actor: "user-a",
			Patch: map[string]json.RawMessage{"checklist": json.RawMessage(`{"invalid":true}`)},
		})
		require.Error(t, err)
		requireCursorUnchanged(ctx, t, uiStore, before)
	})
}

// RunUISnapshotCollectionContract verifies the browser collection authority
// needed by the ported Kata issue list on every storage backend.
func RunUISnapshotCollectionContract(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	store := open(t)
	uiStore, ok := store.(db.UIStore)
	require.True(t, ok, "storage backend must implement db.UIStore")
	ctx := context.Background()
	project := createCursorProject(ctx, t, store)
	parent := createCursorIssue(ctx, t, store, project.ID, "Parent issue")
	child := createCursorIssue(ctx, t, store, project.ID, "Child issue")
	blocker := createCursorIssue(ctx, t, store, project.ID, "Blocker issue")
	blocked := createCursorIssue(ctx, t, store, project.ID, "Blocked issue")
	removed := createCursorIssue(ctx, t, store, project.ID, "Removed child issue")
	createCursorLink(ctx, t, store, child, parent, "parent")
	createCursorLink(ctx, t, store, blocker, blocked, "blocks")
	createCursorLink(ctx, t, store, removed, parent, "parent")
	_, _, changed, err := store.SoftDeleteIssue(ctx, removed.ID, "user-a")
	require.NoError(t, err)
	require.True(t, changed)

	ready, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", Statuses: []string{"ready"},
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{parent.UID, child.UID, blocker.UID}, uiIssueUIDs(ready.Issues))

	parents, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", Relationships: []string{"child"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{parent.UID}, uiIssueUIDs(parents.Issues))

	children, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", Relationships: []string{"parent"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{child.UID}, uiIssueUIDs(children.Issues))

	collection, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open"})
	require.NoError(t, err)
	require.Len(t, collection.CollectionLinks, 2)

	closedLinked := createCursorIssue(ctx, t, store, project.ID, "Closed linked issue")
	createCursorLink(ctx, t, store, parent, closedLinked, "blocks")
	_, _, changed, err = store.CloseIssue(
		ctx, closedLinked.ID, "done", "user-a", "Completed example work", nil,
	)
	require.NoError(t, err)
	require.True(t, changed)

	graph, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", SelectedIssueUID: parent.UID, IncludeGraph: true,
	})
	require.NoError(t, err)
	require.Contains(t, uiIssueUIDs(graph.GraphIssues), closedLinked.UID,
		"graph authority must not inherit the collection's open-status filter")
	require.NotEmpty(t, graph.GraphLinks)
}

// RunUISnapshotViewScopeContract verifies that database-side view predicates
// run before the collection limit and that detail recurrences stay scoped to
// the selected issue's project.
func RunUISnapshotViewScopeContract(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	for _, test := range []struct {
		name     string
		view     string
		metadata map[string]json.RawMessage
	}{
		{name: "today", view: "today", metadata: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-08-01"`),
		}},
		{name: "upcoming", view: "upcoming", metadata: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-08-02"`),
		}},
		{name: "deadlines", view: "deadlines", metadata: map[string]json.RawMessage{
			"deadline_on": json.RawMessage(`"2026-08-03"`),
		}},
	} {
		t.Run(test.name+" filters before limit", func(t *testing.T) {
			store := open(t)
			uiStore := store.(db.UIStore)
			ctx := context.Background()
			project := createCursorProject(ctx, t, store)
			matching, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: "Matching issue", Author: "user-a", Metadata: test.metadata,
			})
			require.NoError(t, err)
			irrelevant := createCursorIssue(ctx, t, store, project.ID, "Newer irrelevant issue")
			addCursorLabel(ctx, t, store, matching, "ready")
			addCursorLabel(ctx, t, store, irrelevant, "ready")

			snapshot, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
				View: test.view, LocalDate: "2026-08-01", TimeZone: "UTC", Limit: 1,
				Labels: []string{"ready"},
			})
			require.NoError(t, err)
			require.Equal(t, []string{matching.UID}, uiIssueUIDs(snapshot.Issues))
		})
	}

	t.Run("inbox filters before limit", func(t *testing.T) {
		store := open(t)
		uiStore := store.(db.UIStore)
		ctx := context.Background()
		inbox := createCursorProject(ctx, t, store)
		_, err := store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
			ProjectID: inbox.ID, Actor: "user-a",
			Patch: map[string]json.RawMessage{"role": json.RawMessage(`"inbox"`)},
		})
		require.NoError(t, err)
		matching := createCursorIssue(ctx, t, store, inbox.ID, "Inbox issue")
		other, err := store.CreateProject(ctx, "example-workspace")
		require.NoError(t, err)
		irrelevant := createCursorIssue(ctx, t, store, other.ID, "Newer irrelevant issue")
		addCursorLabel(ctx, t, store, matching, "ready")
		addCursorLabel(ctx, t, store, irrelevant, "ready")

		snapshot, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			View: "inbox", Labels: []string{"ready"}, Limit: 1,
		})
		require.NoError(t, err)
		require.Equal(t, []string{matching.UID}, uiIssueUIDs(snapshot.Issues))
	})

	t.Run("selected recurrences use selected project", func(t *testing.T) {
		store := open(t)
		uiStore := store.(db.UIStore)
		ctx := context.Background()
		selectedProject := createCursorProject(ctx, t, store)
		selectedIssue := createCursorIssue(ctx, t, store, selectedProject.ID, "Selected issue")
		otherProject, err := store.CreateProject(ctx, "example-workspace")
		require.NoError(t, err)
		selectedRecurrence, _, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
			ProjectID: selectedProject.ID, Actor: "user-a", Rule: "FREQ=WEEKLY",
			DTStart: "2026-08-01", Timezone: "UTC",
			Template: db.RecurrenceTemplate{Title: "Selected recurrence", Metadata: json.RawMessage(`{}`)},
		})
		require.NoError(t, err)
		_, _, err = store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
			ProjectID: otherProject.ID, Actor: "user-a", Rule: "FREQ=DAILY",
			DTStart: "2026-08-01", Timezone: "UTC",
			Template: db.RecurrenceTemplate{Title: "Other recurrence", Metadata: json.RawMessage(`{}`)},
		})
		require.NoError(t, err)

		snapshot, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			View: "all-open", SelectedIssueUID: selectedIssue.UID,
		})
		require.NoError(t, err)
		require.Len(t, snapshot.Recurrences, 1)
		require.Equal(t, selectedRecurrence.ID, snapshot.Recurrences[0].ID)
	})

	t.Run("archived project relationships are excluded", func(t *testing.T) {
		store := open(t)
		uiStore := store.(db.UIStore)
		ctx := context.Background()
		activeProject := createCursorProject(ctx, t, store)
		archivedProject, err := store.CreateProject(ctx, "example-workspace")
		require.NoError(t, err)
		activeIssue := createCursorIssue(ctx, t, store, activeProject.ID, "Active issue")
		archivedPeer := createCursorIssue(ctx, t, store, archivedProject.ID, "Archived peer")
		createCursorLink(ctx, t, store, activeIssue, archivedPeer, "parent")
		_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
			ProjectID: archivedProject.ID, Actor: "user-a", Force: true,
		})
		require.NoError(t, err)

		filtered, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			View: "all-open", Relationships: []string{"parent"},
		})
		require.NoError(t, err)
		require.Empty(t, filtered.Issues)

		selected, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			View: "all-open", SelectedIssueUID: activeIssue.UID,
		})
		require.NoError(t, err)
		require.Empty(t, selected.SelectedLinks)
	})
}

func createCursorLink(
	ctx context.Context,
	t *testing.T,
	store db.Storage,
	from db.Issue,
	to db.Issue,
	relation string,
) {
	t.Helper()
	_, _, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: from.ID, ToIssueID: to.ID, Type: relation, Author: "user-a",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: from.ID,
		FromShortID: from.ShortID, FromUID: from.UID,
		ToShortID: to.ShortID, ToUID: to.UID, Actor: "user-a",
	})
	require.NoError(t, err)
}

func addCursorLabel(
	ctx context.Context,
	t *testing.T,
	store db.Storage,
	issue db.Issue,
	label string,
) {
	t.Helper()
	_, _, err := store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: label, Actor: "user-a",
	})
	require.NoError(t, err)
}

func uiIssueUIDs(issues []db.UIIssue) []string {
	uids := make([]string, len(issues))
	for index := range issues {
		uids[index] = issues[index].UID
	}
	return uids
}

func cursorProjectCreate(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	_, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
}

func cursorProjectRename(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	project := createCursorProject(ctx, t, store)
	_, err := store.RenameProject(ctx, project.ID, "example-workspace")
	require.NoError(t, err)
}

func cursorProjectHardDelete(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	project := createCursorProject(ctx, t, store)
	_, err := store.HardDeleteProject(ctx, project.ID)
	require.NoError(t, err)
}

func cursorIssue(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	project := createCursorProject(ctx, t, store)
	createCursorIssue(ctx, t, store, project.ID, "Example issue")
}

func cursorMetadata(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	issue := createCursorFixtureIssue(ctx, t, store)
	out, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "user-a",
		Patch: map[string]json.RawMessage{"scheduled_on": json.RawMessage(`"2026-08-01"`)},
	})
	require.NoError(t, err)
	require.True(t, out.Changed)
}

func cursorOwner(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	issue := createCursorFixtureIssue(ctx, t, store)
	owner := "user-a"
	_, _, changed, err := store.UpdateOwner(ctx, issue.ID, &owner, "user-a")
	require.NoError(t, err)
	require.True(t, changed)
}

func cursorLabel(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	issue := createCursorFixtureIssue(ctx, t, store)
	_, _, err := store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "ready", Actor: "user-a",
	})
	require.NoError(t, err)
}

func cursorComment(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	issue := createCursorFixtureIssue(ctx, t, store)
	_, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "user-a", Body: "Neutral comment",
	})
	require.NoError(t, err)
}

func cursorLink(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	project := createCursorProject(ctx, t, store)
	from := createCursorIssue(ctx, t, store, project.ID, "First issue")
	to := createCursorIssue(ctx, t, store, project.ID, "Second issue")
	_, _, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: from.ID, ToIssueID: to.ID, Type: "related", Author: "user-a",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: from.ID,
		FromShortID: from.ShortID, FromUID: from.UID,
		ToShortID: to.ShortID, ToUID: to.UID, Actor: "user-a",
	})
	require.NoError(t, err)
}

func cursorChecklist(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	issue := createCursorFixtureIssue(ctx, t, store)
	out, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "user-a",
		Patch: map[string]json.RawMessage{
			"checklist": json.RawMessage(`[{"id":"01K1KATA000000000000000001","text":"Review","done":false}]`),
		},
	})
	require.NoError(t, err)
	require.True(t, out.Changed)
}

func cursorClose(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	issue := createCursorFixtureIssue(ctx, t, store)
	_, _, changed, err := store.CloseIssue(ctx, issue.ID, "done", "user-a", "Completed", nil)
	require.NoError(t, err)
	require.True(t, changed)
}

func cursorReopen(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	issue := createCursorFixtureIssue(ctx, t, store)
	_, _, _, err := store.CloseIssue(ctx, issue.ID, "done", "user-a", "Completed", nil)
	require.NoError(t, err)
	_, _, changed, err := store.ReopenIssue(ctx, issue.ID, "user-a")
	require.NoError(t, err)
	require.True(t, changed)
}

func cursorDelete(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	issue := createCursorFixtureIssue(ctx, t, store)
	_, _, changed, err := store.SoftDeleteIssue(ctx, issue.ID, "user-a")
	require.NoError(t, err)
	require.True(t, changed)
}

func cursorRestore(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	issue := createCursorFixtureIssue(ctx, t, store)
	_, _, _, err := store.SoftDeleteIssue(ctx, issue.ID, "user-a")
	require.NoError(t, err)
	_, _, changed, err := store.RestoreIssue(ctx, issue.ID, "user-a")
	require.NoError(t, err)
	require.True(t, changed)
}

func cursorRecurrence(ctx context.Context, t *testing.T, store db.Storage) {
	t.Helper()
	project := createCursorProject(ctx, t, store)
	_, _, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: project.ID, Actor: "user-a", Rule: "FREQ=WEEKLY",
		DTStart: "2026-08-01", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "Weekly review", Metadata: json.RawMessage(`{}`)},
	})
	require.NoError(t, err)
}

func createCursorFixtureIssue(ctx context.Context, t *testing.T, store db.Storage) db.Issue {
	t.Helper()
	project := createCursorProject(ctx, t, store)
	return createCursorIssue(ctx, t, store, project.ID, "Example issue")
}

func createCursorProject(ctx context.Context, t *testing.T, store db.Storage) db.Project {
	t.Helper()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	return project
}

func createCursorIssue(ctx context.Context, t *testing.T, store db.Storage, projectID int64, title string) db.Issue {
	t.Helper()
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projectID, Title: title, Author: "user-a",
	})
	require.NoError(t, err)
	return issue
}

func requireCursorUnchanged(ctx context.Context, t *testing.T, store db.UIStore, want int64) {
	t.Helper()
	got, err := store.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Equal(t, want, got, "no-op or rejected write must not advance cursor")
}
