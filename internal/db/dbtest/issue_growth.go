package dbtest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/growthtest"
)

// RunCreateIssueHistoryGrowth measures issue creation and reads that verify the saved issue and event as ordinary history grows.
func RunCreateIssueHistoryGrowth(t *testing.T, open func(*testing.T) db.Storage, seed func(*testing.T, db.Storage, db.Project, db.Issue, int)) {
	t.Helper()
	growthtest.Run(t, growthtest.Check{
		Small: 21_000, Large: 210_000,
		MaxRatio: 2, Floor: float64(10 * time.Microsecond),
		Measure: growthtest.Fastest(9),
		Build: func(_ testing.TB, size int) (run, reset func()) {
			store := open(t)
			project, err := store.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			historyIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01M60000000000000000000000", Title: "History issue", Author: "agent"})
			require.NoError(t, err)
			// A real move keeps PostgreSQL's reset-history join from ending at an empty move index.
			target, err := store.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: historyIssue.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: historyIssue.Revision, Actor: "agent"})
			require.NoError(t, err)
			seed(t, store, project, historyIssue, size)

			// Choose a fresh UID outside each measurement, including the warm-up.
			var issueUID string
			sample := 0
			return func() {
					issue, event, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
						ProjectID: project.ID, UID: issueUID, Title: "New issue", Author: "agent",
					})
					require.NoError(t, err)
					require.Equal(t, issueUID, issue.UID)
					require.Equal(t, project.ID, issue.ProjectID)
					require.Equal(t, "New issue", issue.Title)
					require.NotEmpty(t, issue.ShortID)
					persisted, err := store.IssueByID(t.Context(), issue.ID)
					require.NoError(t, err)
					require.Equal(t, issue, persisted)
					events, err := store.EventsByUIDs(t.Context(), project.ID, []string{event.UID})
					require.NoError(t, err)
					require.Len(t, events, 1)
					require.Equal(t, "issue.created", events[0].Type)
					require.Equal(t, new(issueUID), events[0].IssueUID)
				}, func() {
					sample++
					issueUID = fmt.Sprintf("01M5%022d", sample)
				}
		},
	})
}
