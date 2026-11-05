package sqlitestore_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/growthtest"
)

// Ordinary project history must not make short-ID reservation checks more expensive.
func TestCreateIssueHistoryGrowth(t *testing.T) {
	growthtest.Run(t, growthtest.Check{
		Small: 21_000, Large: 210_000,
		MaxRatio: 2, Floor: float64(10 * time.Microsecond),
		Measure: growthtest.Fastest(9),
		Build: func(_ testing.TB, size int) (run, reset func()) {
			store := openTestDB(t)
			project, err := store.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			historyIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01M60000000000000000000000", Title: "History issue", Author: "agent"})
			require.NoError(t, err)
			// A real move keeps PostgreSQL's reset-history join from ending at an empty move index.
			target, err := store.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: historyIssue.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: historyIssue.Revision, Actor: "agent"})
			require.NoError(t, err)
			_, err = store.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (
    VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?
)
INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
SELECT printf('01M4%022d',x),?,?,?,?,?,'issue.updated','agent','{}',x,0,? FROM n`, size, store.InstanceUID(), project.ID, project.Name, historyIssue.ID, historyIssue.UID, strings.Repeat("a", 64))
			require.NoError(t, err)

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
					var eventType, eventIssueUID string
					require.NoError(t, store.QueryRowContext(t.Context(),
						"SELECT type,issue_uid FROM events WHERE id=?", event.ID,
					).Scan(&eventType, &eventIssueUID))
					require.Equal(t, "issue.created", eventType)
					require.Equal(t, issueUID, eventIssueUID)
				}, func() {
					sample++
					issueUID = fmt.Sprintf("01M5%022d", sample)
				}
		},
	})
}
