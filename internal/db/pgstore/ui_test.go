package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/testenv"
)

func TestUISnapshotMutationCursorContract(t *testing.T) {
	dbtest.RunUISnapshotMutationCursorContract(t, func(t *testing.T) db.Storage {
		return openUIStore(t)
	})
}

func TestUISnapshotCompletenessContract(t *testing.T) {
	dbtest.RunUISnapshotCompletenessContract(t, func(t *testing.T) db.Storage {
		return openUIStore(t)
	})
}

func TestUISnapshotAuthorityReuseContract(t *testing.T) {
	dbtest.RunUISnapshotAuthorityReuseContract(t, func(t *testing.T) db.Storage {
		return openUIStore(t)
	})
}

func TestUISnapshotAuthorityReuseSkipsProjectStats(t *testing.T) {
	ctx := context.Background()
	store := openUIStore(t)
	project := createUIProject(t, store, "example-project")
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Selected issue", Author: "user-a",
	})
	require.NoError(t, err)
	full, err := store.ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open"})
	require.NoError(t, err)

	statsReads := 0
	store.uiProjectStatsRead = func() { statsReads++ }
	reused, err := store.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", SelectedIssueUID: issue.UID, ReuseAuthorityCursor: &full.Cursor,
	})
	require.NoError(t, err)
	require.True(t, reused.AuthorityReused)
	require.Zero(t, statsReads)
}

func TestUISnapshotReadsProjectStatsInOneBatch(t *testing.T) {
	ctx := context.Background()
	store := openUIStore(t)
	for _, name := range []string{"example-project", "example-workspace", "spoke-project"} {
		project := createUIProject(t, store, name)
		_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Open issue", Author: "user-a",
		})
		require.NoError(t, err)
	}

	statsReads := 0
	store.uiProjectStatsRead = func() { statsReads++ }
	snapshot, err := store.ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open"})
	require.NoError(t, err)
	require.Equal(t, 1, statsReads)
	require.Len(t, snapshot.Projects, 3)
	for _, project := range snapshot.Projects {
		assert.Equal(t, 1, project.Stats.Open)
		assert.Zero(t, project.Stats.Closed)
		assert.NotNil(t, project.Stats.LastEventAt)
	}
}

func TestUISnapshotReadsCollectionLinkDetailsInOneBatch(t *testing.T) {
	ctx := context.Background()
	store := openUIStore(t)
	project := createUIProject(t, store, "example-project")
	issues := make([]db.Issue, 4)
	for index := range issues {
		var err error
		issues[index], _, err = store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Linked issue", Author: "user-a",
		})
		require.NoError(t, err)
	}
	for index := 1; index < len(issues); index++ {
		_, _, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
			FromIssueID: issues[index].ID, ToIssueID: issues[0].ID,
			Type: "blocks", Author: "user-a",
		}, db.LinkEventParams{
			EventType: "issue.linked", EventIssueID: issues[index].ID,
			FromShortID: issues[index].ShortID, FromUID: issues[index].UID,
			ToShortID: issues[0].ShortID, ToUID: issues[0].UID, Actor: "user-a",
		})
		require.NoError(t, err)
	}

	detailReads := 0
	store.uiLinkDetailRead = func() { detailReads++ }
	snapshot, err := store.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", ProjectUID: project.UID,
	})
	require.NoError(t, err)
	require.Len(t, snapshot.CollectionLinks, 3)
	require.Equal(t, 1, detailReads)
}

func TestUISnapshotCollectionContract(t *testing.T) {
	dbtest.RunUISnapshotCollectionContract(t, func(t *testing.T) db.Storage {
		return openUIStore(t)
	})
}

func TestUISnapshotViewScopeContract(t *testing.T) {
	dbtest.RunUISnapshotViewScopeContract(t, func(t *testing.T) db.Storage {
		return openUIStore(t)
	})
}

func TestReadUILabelsForIssues(t *testing.T) {
	ctx := context.Background()
	store := openUIStore(t)
	project := createUIProject(t, store, "example-project")
	first, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "First issue", Author: "user-a",
	})
	require.NoError(t, err)
	second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Second issue", Author: "user-a",
	})
	require.NoError(t, err)
	for _, label := range []string{"beta", "alpha"} {
		_, _, err = store.AddLabelAndEvent(ctx, first.ID, db.LabelEventParams{
			EventType: "issue.labeled", Label: label, Actor: "user-a",
		})
		require.NoError(t, err)
	}
	tx, err := store.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	issues := []db.UIIssue{{Issue: first}, {Issue: second}}

	require.NoError(t, readUILabelsForIssues(ctx, tx, issues))
	require.Equal(t, []string{"alpha", "beta"}, issues[0].Labels)
	require.Empty(t, issues[1].Labels)
}

func TestReadUISnapshotConsistent(t *testing.T) {
	ctx := context.Background()
	store := openUIStore(t)
	first := createUIProject(t, store, "example-project")
	second := createUIProject(t, store, "example-workspace")
	owner := "user-a"
	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: first.ID,
		Title:     "Parent issue",
		Body:      "Parent details",
		Author:    "user-a",
		Owner:     &owner,
		Metadata: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-08-01"`),
		},
	})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: first.ID, Title: "Child issue", Author: "user-a",
	})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: second.ID, Title: "Other project issue", Author: "user-a",
	})
	require.NoError(t, err)
	_, _, err = store.AddLabelAndEvent(ctx, parent.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "ready", Actor: "user-a",
	})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: parent.ID, Author: "user-a", Body: "A neutral comment",
	})
	require.NoError(t, err)
	_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "user-a",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: child.ID,
		FromShortID: child.ShortID, FromUID: child.UID,
		ToShortID: parent.ShortID, ToUID: parent.UID, Actor: "user-a",
	})
	require.NoError(t, err)
	_, _, err = store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: first.ID,
		Actor:     "user-a",
		Rule:      "FREQ=WEEKLY",
		DTStart:   "2026-08-01",
		Timezone:  "America/Chicago",
		Template: db.RecurrenceTemplate{
			Title: "Weekly review", Labels: []string{"ready"}, Metadata: json.RawMessage(`{}`),
		},
	})
	require.NoError(t, err)

	beforeCursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	var afterCursor int64
	store.uiReadStage = func(ctx context.Context) error {
		created, event, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: first.ID, Title: "Concurrent issue", Author: "user-a",
		})
		if err != nil {
			return err
		}
		require.NotEmpty(t, created.UID)
		afterCursor = event.ID
		return nil
	}

	snapshot, err := store.ReadUISnapshot(ctx, db.UISnapshotQuery{
		ProjectUID: first.UID, SelectedIssueUID: parent.UID,
		IncludeGraph: true, IncludeHistory: true,
	})
	require.NoError(t, err)
	require.Greater(t, afterCursor, beforeCursor)
	before := len(snapshot.Issues) == 2 && snapshot.Cursor == beforeCursor
	after := len(snapshot.Issues) == 3 && snapshot.Cursor == afterCursor
	assert.True(t, before || after,
		"snapshot must be entirely before or after concurrent commit: issues=%d cursor=%d before=%d after=%d",
		len(snapshot.Issues), snapshot.Cursor, beforeCursor, afterCursor)
	require.Len(t, snapshot.Projects, 2)
	require.NotNil(t, snapshot.SelectedIssue)
	assert.Equal(t, parent.UID, snapshot.SelectedIssue.UID)
	require.Len(t, snapshot.Comments, 1)
	assert.Equal(t, "ready", snapshot.SelectedLabels[0].Label)
	require.Len(t, snapshot.SelectedLinks, 1)
	require.Len(t, snapshot.Recurrences, 1)
	assert.NotEmpty(t, snapshot.History)
	assert.NotEmpty(t, snapshot.GraphIssues)
	assert.NotEmpty(t, snapshot.GraphLinks)
}

func TestReadUIReferences(t *testing.T) {
	ctx := context.Background()
	store := openUIStore(t)
	first := createUIProject(t, store, "example-project")
	createUIProject(t, store, "example-workspace")
	owner := "user-a"
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: first.ID, Title: "Example result", Author: "user-a", Owner: &owner,
	})
	require.NoError(t, err)
	_, _, err = store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "ready", Actor: "user-a",
	})
	require.NoError(t, err)

	references, err := store.ReadUIReferences(ctx, db.UIReferencesQuery{Query: "example", Limit: 20})
	require.NoError(t, err)
	require.Len(t, references.Projects, 2)
	require.Len(t, references.Issues, 1)
	assert.Equal(t, "example-project#"+issue.ShortID, references.Issues[0].QualifiedID)
	assert.Equal(t, []string{"user-a"}, references.Owners)
	assert.Equal(t, []string{"ready"}, references.Labels)
	assert.Greater(t, references.Cursor, int64(0))
}

func TestReadUIReferencesFiltersIssueUIDs(t *testing.T) {
	ctx := context.Background()
	store := openUIStore(t)
	firstProject := createUIProject(t, store, "alpha-project")
	secondProject := createUIProject(t, store, "beta-project")
	first, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: firstProject.ID, Title: "First selected issue", Author: "user-a",
	})
	require.NoError(t, err)
	ignored, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: firstProject.ID, Title: "Ignored issue", Author: "user-a",
	})
	require.NoError(t, err)
	for index := range 201 {
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: firstProject.ID, Title: fmt.Sprintf("Ignored issue %03d", index), Author: "user-a",
		})
		require.NoError(t, err)
	}
	second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: secondProject.ID, Title: "Second selected issue", Author: "user-a",
	})
	require.NoError(t, err)
	deleted, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: firstProject.ID, Title: "Deleted selected issue", Author: "user-a",
	})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, deleted.ID, "user-a")
	require.NoError(t, err)
	archivedProject := createUIProject(t, store, "archived-project")
	archived, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: archivedProject.ID, Title: "Archived selected issue", Author: "user-a",
	})
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: archivedProject.ID, Actor: "user-a", Force: true,
	})
	require.NoError(t, err)

	hydration, err := store.ReadUIReferenceHydration(ctx, db.UIReferencesQuery{
		Query:     "First selected",
		IssueUIDs: []string{second.UID, first.UID, deleted.UID, archived.UID, second.UID},
		Limit:     1,
	})
	require.NoError(t, err)
	require.Equal(t, []string{first.UID, second.UID}, hydration.ResolvedUIDs)
	require.Equal(t, []int64{firstProject.ID, secondProject.ID}, hydration.ProjectIDs)
	require.Len(t, hydration.References.Issues, 1)
	require.Equal(t, first.UID, hydration.References.Issues[0].UID)
	require.Empty(t, hydration.References.Projects)
	require.Empty(t, hydration.References.Owners)
	require.Empty(t, hydration.References.Labels)

	references, err := store.ReadUIReferences(ctx, db.UIReferencesQuery{
		IssueUIDs: []string{second.UID, first.UID, deleted.UID, archived.UID, second.UID}, Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, references.Issues, 2)
	require.Equal(t, []string{first.UID, second.UID}, []string{
		references.Issues[0].UID, references.Issues[1].UID,
	})
	for _, reference := range references.Issues {
		require.NotEqual(t, ignored.UID, reference.UID)
		require.NotEqual(t, deleted.UID, reference.UID)
		require.NotEqual(t, archived.UID, reference.UID)
	}

	projectReferences, err := store.ReadUIReferences(ctx, db.UIReferencesQuery{
		ProjectUID: firstProject.UID,
		IssueUIDs:  []string{first.UID, second.UID},
		Limit:      20,
	})
	require.NoError(t, err)
	require.Len(t, projectReferences.Issues, 1)
	require.Equal(t, first.UID, projectReferences.Issues[0].UID)
}

func openUIStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{Schema: "ui_test", SchemaMode: SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createUIProject(t *testing.T, store *Store, name string) db.Project {
	t.Helper()
	project, err := store.CreateProject(context.Background(), name)
	require.NoError(t, err)
	return project
}
