package sqlitestore_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/growthtest"
)

func TestIssueUIDPrefixGrowth(t *testing.T) {
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
							store := openTestDB(t)
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
							_, err = store.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (
    VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?
)
INSERT INTO issues(uid,short_id,project_id,title,author)
SELECT ?||printf('%022d',x),printf('%06d',x),CASE x%2 WHEN 0 THEN ? ELSE ? END,'Unrelated issue','agent' FROM n`, size, seedPrefix, secondSeedProject, seedProject)
							require.NoError(t, err)
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

func TestIssueUIDPrefixMatchCaseMatching(t *testing.T) {
	store := openTestDB(t)
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01hznq7v000000000000000001", Title: "Prefix match", Author: "agent"})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01HZNQ7V000000000000000003", Title: "Other match", Author: "agent"})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01HZNQ7V000000000000000001", Title: "Case tie", Author: "agent"})
	require.NoError(t, err)
	for _, prefix := range []string{"01HZNQ7V", "01hznq7v"} {
		matches, err := store.IssueUIDPrefixMatch(t.Context(), prefix, 20, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Len(t, matches, 3)
		// Candidate order follows case-insensitive UID values, then original bytes.
		require.Equal(t, "01HZNQ7V000000000000000001", matches[0].UID)
		require.Equal(t, "01hznq7v000000000000000001", matches[1].UID)
		require.Equal(t, "01HZNQ7V000000000000000003", matches[2].UID)
	}
}
