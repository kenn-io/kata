package dbtest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/growthtest"
)

// RunCreateIssueHistoryGrowth checks that issue creation stays flat as ordinary history grows.
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
			historyIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01M60000000000000000000000", Title: "History a", Author: "agent"})
			require.NoError(t, err)
			seed(t, store, project, historyIssue, size)
			// A real move keeps PostgreSQL's reset-history join from ending at an empty move index.
			target, err := store.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: historyIssue.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: historyIssue.Revision, Actor: "agent"})
			require.NoError(t, err)

			// Choose a fresh UID outside each measurement, including the warm-up.
			var issueUID string
			sample := 0
			return func() {
					_, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
						ProjectID: project.ID, UID: issueUID, Title: "New issue", Author: "agent",
					})
					require.NoError(t, err)
				}, func() {
					sample++
					issueUID = fmt.Sprintf("01M5%022d", sample)
				}
		},
	})
}

// RunIssueUIDPrefixGrowth checks bounded candidate lookup across project scopes.
func RunIssueUIDPrefixGrowth(t *testing.T, open func(*testing.T) db.Storage, seed func(*testing.T, db.Storage, int, int, string, int64, int64)) {
	t.Helper()
	for _, scope := range []string{"unrestricted", "one-project", "multiple-projects"} {
		t.Run(scope, func(t *testing.T) {
			for _, kind := range []string{"hit", "missing", "hidden-matches", "ambiguous"} {
				if scope == "unrestricted" && kind == "hidden-matches" {
					continue
				}
				t.Run(kind, func(t *testing.T) {
					growthtest.Run(t, growthtest.Check{
						Small: 10_000, Large: 100_000,
						MaxRatio: 2, Floor: float64(32 * time.Microsecond),
						Measure: growthtest.Fastest(9),
						Build: func(_ testing.TB, size int) (run, reset func()) {
							store := open(t)
							project, err := store.CreateProject(t.Context(), "example-project")
							require.NoError(t, err)
							second, err := store.CreateProject(t.Context(), "second-project")
							require.NoError(t, err)
							hidden, err := store.CreateProject(t.Context(), "hidden-project")
							require.NoError(t, err)
							_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: hidden.ID, UID: "01ZY0000000000000000000001", Title: "Hidden issue", Author: "agent"})
							require.NoError(t, err)
							ctx := t.Context()
							lookupPrefix := "01ZZ"
							if kind == "missing" {
								lookupPrefix = "01ZY"
							}
							switch scope {
							case "one-project":
								ctx = db.WithAuthorizedProjects(ctx, []string{project.UID})
							case "multiple-projects":
								ctx = db.WithAuthorizedProjects(ctx, []string{project.UID, second.UID})
							case "unrestricted":
								if kind == "missing" {
									lookupPrefix = "01ZX"
								}
							}
							seedPrefix, seedProject := "01M4", project.ID
							if kind == "hidden-matches" {
								seedPrefix, seedProject = "01ZZ", hidden.ID
							}
							if kind == "ambiguous" {
								seedPrefix = "01ZZ"
							}
							secondSeedProject := seedProject
							if kind == "ambiguous" && scope == "multiple-projects" {
								secondSeedProject = second.ID
							}
							seed(t, store, size, 0, seedPrefix, secondSeedProject, seedProject)
							if kind == "ambiguous" {
								// Both a broad UID scan and sorting every match must grow here.
								seed(t, store, size, size, "01M4", secondSeedProject, seedProject)
							}
							// Keep the visible hit after the growing set of hidden matches.
							matchProject := project.ID
							if scope == "multiple-projects" {
								matchProject = second.ID
							}
							matchUID := "01ZZ0000000000000000000001"
							if kind == "hidden-matches" || kind == "ambiguous" {
								matchUID = "01ZZ9999999999999999999999"
							}
							_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: matchProject, UID: matchUID, Title: "Matching issue", Author: "agent"})
							require.NoError(t, err)
							limit := 1
							if kind == "ambiguous" {
								limit = 20
							}
							return func() {
								for range 32 {
									matches, err := store.IssueUIDPrefixMatch(ctx, lookupPrefix, limit, db.IncludeDeletedNo)
									require.NoError(t, err)
									switch kind {
									case "hit", "hidden-matches":
										require.Len(t, matches, 1)
										require.Equal(t, matchUID, matches[0].UID)
									case "missing":
										require.Empty(t, matches)
									case "ambiguous":
										require.Len(t, matches, 20)
										for n, match := range matches {
											require.Equal(t, fmt.Sprintf("01ZZ%022d", n+1), match.UID)
										}
									}
								}
							}, nil
						},
					})
				})
			}
		})
	}
}
