package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/growthtest"
)

// RunCreateIssueHistoryGrowth checks that issue creation stays flat as ordinary history grows.
// The caller chooses the cost measure and its minimum meaningful value.
func RunCreateIssueHistoryGrowth(t *testing.T, open func(*testing.T) db.Storage, seed func(*testing.T, db.Storage, db.Project, db.Issue, int), measure growthtest.Measure, floor float64) {
	t.Helper()
	growthtest.Run(t, growthtest.Check{
		Small: 21_000, Large: 210_000,
		MaxRatio: 2, Floor: floor,
		Measure: measure,
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
// The caller chooses the cost measure and its minimum meaningful value.
func RunIssueUIDPrefixGrowth(t *testing.T, open func(*testing.T) db.Storage, seed func(*testing.T, db.Storage, int, int, string, int64, int64), measure growthtest.Measure, floor float64) {
	t.Helper()
	if testing.Short() {
		t.Skip("growthtest: growth checks skip under -short")
	}
	type fixture struct {
		store           db.Storage
		project, second db.Project
	}
	fixtures := make(map[int]fixture, 2)
	// All lookups are read-only, so the cases can share each size's fixture.
	for _, size := range []int{10_000, 100_000} {
		store := open(t)
		project, err := store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		second, err := store.CreateProject(t.Context(), "second-project")
		require.NoError(t, err)
		hidden, err := store.CreateProject(t.Context(), "hidden-project")
		require.NoError(t, err)
		seed(t, store, size, size, "01M4", project.ID, project.ID)
		seed(t, store, size, 0, "01ZZ", second.ID, project.ID)
		seed(t, store, size, 0, "01ZY", hidden.ID, hidden.ID)
		for _, params := range []db.CreateIssueParams{
			{ProjectID: hidden.ID, UID: "01ZQ0000000000000000000001", Title: "Hidden issue", Author: "agent"},
			{ProjectID: project.ID, UID: "01ZX0000000000000000000002", Title: "Matching issue", Author: "agent"},
			{ProjectID: second.ID, UID: "01ZX0000000000000000000001", Title: "Other matching issue", Author: "agent"},
			{ProjectID: project.ID, UID: "01ZY9999999999999999999999", Title: "Visible after hidden matches", Author: "agent"},
			{ProjectID: second.ID, UID: "01ZY9999999999999999999998", Title: "Other visible after hidden matches", Author: "agent"},
		} {
			_, _, err = store.CreateIssue(t.Context(), params)
			require.NoError(t, err)
		}
		fixtures[size] = fixture{store: store, project: project, second: second}
	}
	for _, scope := range []string{"unrestricted", "one-project", "multiple-projects"} {
		t.Run(scope, func(t *testing.T) {
			for _, kind := range []string{"hit", "missing", "hidden-matches", "ambiguous"} {
				if scope == "unrestricted" && kind == "hidden-matches" {
					continue
				}
				t.Run(kind, func(t *testing.T) {
					growthtest.Run(t, growthtest.Check{
						Small: 10_000, Large: 100_000,
						MaxRatio: 2, Floor: floor,
						Measure: measure,
						Build: func(_ testing.TB, size int) (run, reset func()) {
							fixture := fixtures[size]
							ctx := t.Context()
							lookupPrefix, matchUID := "01ZX", "01ZX0000000000000000000001"
							switch scope {
							case "one-project":
								ctx = db.WithAuthorizedProjects(ctx, []string{fixture.project.UID})
								matchUID = "01ZX0000000000000000000002"
							case "multiple-projects":
								ctx = db.WithAuthorizedProjects(ctx, []string{fixture.project.UID, fixture.second.UID})
							}
							limit := 1
							switch kind {
							case "missing":
								lookupPrefix = "01ZQ"
								if scope == "unrestricted" {
									lookupPrefix = "01ZP"
								}
							case "hidden-matches":
								lookupPrefix, matchUID = "01ZY", "01ZY9999999999999999999998"
								if scope == "one-project" {
									matchUID = "01ZY9999999999999999999999"
								}
							case "ambiguous":
								lookupPrefix, limit = "01ZZ", 20
							}
							return func() {
								for range 32 {
									matches, err := fixture.store.IssueUIDPrefixMatch(ctx, lookupPrefix, limit, db.IncludeDeletedNo)
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
											suffix := n + 1
											if scope == "one-project" {
												suffix = n*2 + 1
											}
											require.Equal(t, fmt.Sprintf("01ZZ%022d", suffix), match.UID)
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
