package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
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
	tx, err := store.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
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

func TestReadUISnapshotPreservesUnresolvedGraphEndpoints(t *testing.T) {
	ctx := context.Background()
	store := openUIStore(t)
	project := createUIProject(t, store, "example-project")
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Source issue", Author: "user-a",
	})
	require.NoError(t, err)
	missingUID := "01J00000000000000000000009"
	_, err = store.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx,
		`INSERT INTO links(from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		source.ID, 9001, source.UID, missingUID, "blocks", "user-a")
	require.NoError(t, err)

	snapshot, err := store.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", SelectedIssueUID: source.UID, IncludeGraph: true,
	})
	require.NoError(t, err)
	require.Equal(t, []db.UIGraphEdge{{
		FromUID: source.UID, ToUID: missingUID, Kind: "blocks", Layout: true,
	}}, snapshot.GraphEdges)
	require.Equal(t, []db.UIGraphUnresolvedRef{{
		UID: missingUID, Side: "to", Kind: "blocks", OtherUID: source.UID,
	}}, snapshot.GraphUnresolvedRefs)
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

func openUIStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
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
