package daemon_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

func TestScopedStorageCollectionAndRecurrenceContracts(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store db.Storage
			var err error
			if backend == "postgres" {
				if testing.Short() {
					t.Skip("requires postgres testcontainer")
				}
				dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
				t.Cleanup(cleanup)
				store, err = pgstore.Open(t.Context(), dsn)
			} else {
				store, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			project, err := store.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			createIssue := func(title string, parent *db.Issue) db.Issue {
				params := db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "coordinator"}
				if parent != nil {
					params.Links = []db.InitialLink{{Type: "parent", ToNumber: parent.ID}}
				}
				issue, _, err := store.CreateIssue(t.Context(), params)
				require.NoError(t, err)
				return issue
			}
			root := createIssue("Root", nil)
			child := createIssue("Needle inside", &root)
			createIssue("Needle outside", nil)
			scope := &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID}
			listed, err := store.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID, Limit: 1, IssueScope: scope})
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.Equal(t, child.ID, listed[0].ID)
			ready, err := store.ReadyIssues(t.Context(), project.ID, 1, db.ReadyIssuesFilter{IssueScope: scope})
			require.NoError(t, err)
			require.Len(t, ready, 1)
			require.Equal(t, child.ID, ready[0].ID)
			hits, err := store.SearchFTS(t.Context(), db.SearchFTSParams{ProjectID: project.ID, Query: "Needle", Limit: 1, IssueScope: scope})
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, child.ID, hits[0].Issue.ID)

			_, err = store.CreateRecurrenceForIssue(t.Context(), db.CreateRecurrenceForIssueIn{
				IssueID: child.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: project.ID, Actor: "coordinator", Rule: "FREQ=WEEKLY", DTStart: "2030-01-01", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Recurring work"}},
			})
			require.NoError(t, err)
			_, _, _, err = store.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: child.ID, ExpectedProjectID: project.ID, Reason: "done", Actor: "worker", Message: "Completed work", DisallowRecurrenceEffects: true})
			require.ErrorIs(t, err, db.ErrRecurrenceEffectsForbidden)
			current, err := store.IssueByID(t.Context(), child.ID)
			require.NoError(t, err)
			require.Equal(t, "open", current.Status)
			issues, err := store.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID})
			require.NoError(t, err)
			require.Len(t, issues, 3, "rejected close must not materialize another occurrence")
			parent, err := store.ParentOf(t.Context(), child.ID)
			require.NoError(t, err)
			require.NoError(t, store.DeleteLinkByID(t.Context(), parent.ID))
			snapshot, err := store.(db.UIStore).ReadUISnapshot(t.Context(), db.UISnapshotQuery{
				View: "all-open", ProjectUID: project.UID, IssueScope: scope, SelectedIssueUID: child.UID, IncludeHistory: true,
				AllowedIssueIDs: []int64{root.ID, child.ID},
			})
			require.NoError(t, err)
			require.Len(t, snapshot.Issues, 1, "read transaction must refresh stale subtree candidates")
			require.Equal(t, root.UID, snapshot.Issues[0].UID)
			require.Nil(t, snapshot.SelectedIssue)
			require.Empty(t, snapshot.Comments)
			require.Empty(t, snapshot.History)
			references, err := store.(db.UIStore).ReadUIReferences(t.Context(), db.UIReferencesQuery{
				ProjectUID: project.UID, IssueScope: scope, AllowedIssueIDs: []int64{root.ID, child.ID},
			})
			require.NoError(t, err)
			require.Len(t, references.Issues, 1)
			require.Equal(t, root.UID, references.Issues[0].UID)

		})
	}
}
