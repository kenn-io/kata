package dbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

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
	createWithMetadata := func(title string, metadata map[string]json.RawMessage) db.Issue {
		issue, _, createErr := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: title, Author: "user-a", Metadata: metadata,
		})
		require.NoError(t, createErr)
		return issue
	}
	today := time.Now().UTC()
	someday := createWithMetadata("Someday issue", map[string]json.RawMessage{
		"someday": json.RawMessage(`true`),
	})
	futureScheduled := createWithMetadata("Future scheduled issue", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(fmt.Sprintf("%q", today.AddDate(0, 0, 1).Format(time.DateOnly))),
	})
	somedayFalse := createWithMetadata("Someday false issue", map[string]json.RawMessage{
		"someday": json.RawMessage(`false`),
	})
	todayScheduled := createWithMetadata("Today scheduled issue", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(fmt.Sprintf("%q", today.Format(time.DateOnly))),
	})
	pastScheduled := createWithMetadata("Past scheduled issue", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(fmt.Sprintf("%q", today.AddDate(-1, 0, 0).Format(time.DateOnly))),
	})
	createCursorLink(ctx, t, store, child, parent, "parent")
	createCursorLink(ctx, t, store, blocker, blocked, "blocks")
	createCursorLink(ctx, t, store, removed, parent, "parent")
	_, _, changed, err := store.SoftDeleteIssue(ctx, removed.ID, "user-a")
	require.NoError(t, err)
	require.True(t, changed)

	ready, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", Statuses: []string{"ready"}, ReadyAt: today.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		parent.UID, child.UID, blocker.UID, somedayFalse.UID, todayScheduled.UID, pastScheduled.UID,
	}, uiIssueUIDs(ready.Issues))
	require.NotContains(t, uiIssueUIDs(ready.Issues), someday.UID)
	require.NotContains(t, uiIssueUIDs(ready.Issues), futureScheduled.UID)

	rolled, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", Statuses: []string{"ready"},
		ReadyAt: today.AddDate(0, 0, 1).Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	require.Contains(t, uiIssueUIDs(rolled.Issues), futureScheduled.UID)
	require.NotContains(t, uiIssueUIDs(rolled.Issues), someday.UID)

	readyAt := time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)
	westScheduled := createWithMetadata("West-zone scheduled issue", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(`"2026-09-01T09:00"`),
		"timezone":     json.RawMessage(`"America/Los_Angeles"`),
	})
	eastScheduled := createWithMetadata("East-zone scheduled issue", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(`"2026-09-01T09:00"`),
		"timezone":     json.RawMessage(`"Asia/Tokyo"`),
	})
	dueInstant := createWithMetadata("Due instant", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(`"2026-09-01T00:30:00Z"`),
	})
	futureInstant := createWithMetadata("Future instant", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(`"2026-09-01T00:31:00Z"`),
	})
	timed, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", Statuses: []string{"ready"}, ReadyAt: readyAt.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	require.NotContains(t, uiIssueUIDs(timed.Issues), westScheduled.UID)
	require.Contains(t, uiIssueUIDs(timed.Issues), eastScheduled.UID)
	require.Contains(t, uiIssueUIDs(timed.Issues), dueInstant.UID)
	require.NotContains(t, uiIssueUIDs(timed.Issues), futureInstant.UID)
	defaultScheduled := createWithMetadata("Default-zone scheduled issue", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(`"2026-09-01"`),
	})
	defaultZoned, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", Statuses: []string{"ready"}, ReadyAt: readyAt.Format(time.RFC3339Nano),
		DefaultTimezone: "America/Los_Angeles",
	})
	require.NoError(t, err)
	require.NotContains(t, uiIssueUIDs(defaultZoned.Issues), defaultScheduled.UID)
	require.Contains(t, uiIssueUIDs(defaultZoned.Issues), eastScheduled.UID,
		"an issue timezone must override the daemon default")

	limitDue := createWithMetadata("Limit due issue", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(`"2026-09-01T00:30:00Z"`),
	})
	createWithMetadata("Limit parked issue", map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(`"2026-09-01T00:31:00Z"`),
	})
	limited, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", Statuses: []string{"ready"}, ReadyAt: readyAt.Format(time.RFC3339Nano), Limit: 1,
	})
	require.NoError(t, err)
	require.Equal(t, []string{limitDue.UID}, uiIssueUIDs(limited.Issues))

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

	t.Run("timed schedules use the browser calendar before limit", func(t *testing.T) {
		store := open(t)
		uiStore := store.(db.UIStore)
		ctx := context.Background()
		project := createCursorProject(ctx, t, store)
		previousDay, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Previous browser day", Author: "user-a",
			Metadata: map[string]json.RawMessage{
				"scheduled_on": json.RawMessage(`"2026-08-01T00:30:00Z"`),
			},
		})
		require.NoError(t, err)
		nextDay, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Next browser day", Author: "user-a",
			Metadata: map[string]json.RawMessage{
				"scheduled_on": json.RawMessage(`"2026-08-01T07:30:00Z"`),
			},
		})
		require.NoError(t, err)

		today, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			View: "today", LocalDate: "2026-07-31", TimeZone: "America/Los_Angeles", Limit: 1,
		})
		require.NoError(t, err)
		require.Len(t, today.Issues, 1)
		require.Equal(t, previousDay.UID, today.Issues[0].UID)
		require.Equal(t, "2026-07-31", today.Issues[0].ScheduledOnDate)

		upcoming, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			View: "upcoming", LocalDate: "2026-07-31", TimeZone: "America/Los_Angeles", Limit: 1,
		})
		require.NoError(t, err)
		require.Len(t, upcoming.Issues, 1)
		require.Equal(t, nextDay.UID, upcoming.Issues[0].UID)
		require.Equal(t, "2026-08-01", upcoming.Issues[0].ScheduledOnDate)
	})

	t.Run("timed deadlines use the browser calendar before limit", func(t *testing.T) {
		store := open(t)
		uiStore := store.(db.UIStore)
		ctx := context.Background()
		project := createCursorProject(ctx, t, store)
		previousDay, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Previous browser deadline", Author: "user-a",
			Metadata: map[string]json.RawMessage{
				"deadline_on": json.RawMessage(`"2026-08-01T00:30:00Z"`),
			},
		})
		require.NoError(t, err)
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Next browser deadline", Author: "user-a",
			Metadata: map[string]json.RawMessage{
				"deadline_on": json.RawMessage(`"2026-08-01T07:30:00Z"`),
			},
		})
		require.NoError(t, err)

		today, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			View: "today", LocalDate: "2026-07-31", TimeZone: "America/Los_Angeles", Limit: 1,
		})
		require.NoError(t, err)
		require.Len(t, today.Issues, 1)
		require.Equal(t, previousDay.UID, today.Issues[0].UID)
		require.Equal(t, "2026-07-31", today.Issues[0].DeadlineOnDate)
	})

	t.Run("deadline projection does not inherit a recurrence timezone", func(t *testing.T) {
		store := open(t)
		uiStore := store.(db.UIStore)
		ctx := context.Background()
		project := createCursorProject(ctx, t, store)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Recurrence deadline", Author: "user-a",
			Metadata: map[string]json.RawMessage{
				"deadline_on": json.RawMessage(`"2026-08-01T23:30"`),
			},
		})
		require.NoError(t, err)
		_, err = store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{
			IssueID: issue.ID,
			Recurrence: db.CreateRecurrenceIn{
				ProjectID: project.ID, Actor: "user-a", Rule: "FREQ=DAILY;COUNT=2",
				DTStart: "2026-08-01", Timezone: "America/Los_Angeles",
				Template: db.RecurrenceTemplate{Title: issue.Title},
			},
		})
		require.NoError(t, err)
		_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID: issue.ID, Actor: "user-a",
			Patch: map[string]json.RawMessage{
				"scheduled_on": json.RawMessage(`null`),
				"timezone":     json.RawMessage(`null`),
			},
		})
		require.NoError(t, err)

		snapshot, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			ProjectUID: project.UID, View: "deadlines", LocalDate: "2026-08-01",
			TimeZone: "UTC", DefaultTimezone: "UTC",
		})
		require.NoError(t, err)
		require.Len(t, snapshot.Issues, 1)
		require.Equal(t, "2026-08-01", snapshot.Issues[0].DeadlineOnDate)

		today, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			ProjectUID: project.UID, View: "today", LocalDate: "2026-08-01",
			TimeZone: "UTC", DefaultTimezone: "UTC",
		})
		require.NoError(t, err)
		require.Len(t, today.Issues, 1,
			"Today must evaluate the deadline with the daemon timezone, not the recurrence timezone")
		require.Equal(t, issue.UID, today.Issues[0].UID)
	})

	t.Run("non-calendar views project deadline dates", func(t *testing.T) {
		store := open(t)
		uiStore := store.(db.UIStore)
		ctx := context.Background()
		project := createCursorProject(ctx, t, store)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "All-open deadline", Author: "user-a",
			Metadata: map[string]json.RawMessage{
				"deadline_on": json.RawMessage(`"2026-09-01T00:30:00Z"`),
			},
		})
		require.NoError(t, err)

		snapshot, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			ProjectUID: project.UID, View: "all-open", TimeZone: "America/Los_Angeles",
			DefaultTimezone: "UTC",
		})
		require.NoError(t, err)
		require.Len(t, snapshot.Issues, 1)
		require.Equal(t, issue.UID, snapshot.Issues[0].UID)
		require.Equal(t, "2026-08-31", snapshot.Issues[0].DeadlineOnDate)
	})

	t.Run("selected issues project timed deadline dates", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			metadata map[string]json.RawMessage
		}{
			{name: "UTC instant", metadata: map[string]json.RawMessage{
				"deadline_on": json.RawMessage(`"2026-09-01T00:30:00Z"`),
			}},
			{name: "local time", metadata: map[string]json.RawMessage{
				"deadline_on": json.RawMessage(`"2026-09-01T09:00"`),
				"timezone":    json.RawMessage(`"Asia/Tokyo"`),
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				store := open(t)
				uiStore := store.(db.UIStore)
				ctx := context.Background()
				project := createCursorProject(ctx, t, store)
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: project.ID, Title: "Selected deadline", Author: "user-a",
					Metadata: test.metadata,
				})
				require.NoError(t, err)

				snapshot, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
					ProjectUID: project.UID, View: "all-open", SelectedIssueUID: issue.UID,
					TimeZone: "America/Los_Angeles", DefaultTimezone: "UTC",
				})
				require.NoError(t, err)
				require.NotNil(t, snapshot.SelectedIssue)
				require.Equal(t, "2026-08-31", snapshot.SelectedIssue.DeadlineOnDate)
			})
		}
	})

	t.Run("ready uses linked recurrence timezone for legacy issues", func(t *testing.T) {
		store := open(t)
		uiStore := store.(db.UIStore)
		ctx := context.Background()
		project := createCursorProject(ctx, t, store)
		issue := createCursorIssue(ctx, t, store, project.ID, "Legacy recurrence issue")
		_, err := store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{
			IssueID: issue.ID,
			Recurrence: db.CreateRecurrenceIn{
				ProjectID: project.ID, Actor: "user-a", Rule: "FREQ=DAILY;COUNT=2",
				DTStart: "2026-08-01", Timezone: "America/Los_Angeles",
				Template: db.RecurrenceTemplate{Title: issue.Title},
			},
		})
		require.NoError(t, err)
		_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID: issue.ID, Actor: "user-a",
			Patch: map[string]json.RawMessage{"timezone": json.RawMessage(`null`)},
		})
		require.NoError(t, err)

		snapshot, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
			ProjectUID: project.UID, Status: "ready",
			ReadyAt: "2026-08-01T00:30:00Z", DefaultTimezone: "UTC",
		})
		require.NoError(t, err)
		require.Empty(t, snapshot.Issues,
			"Los Angeles is still on the previous date when the UTC fallback date opens")
	})

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
