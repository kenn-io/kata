package sqlitestore_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/growthtest"
)

func TestIssueUIDPrefixGrowth(t *testing.T) {
	for _, scope := range []string{"unrestricted", "one-project", "multiple-projects"} {
		t.Run(scope, func(t *testing.T) {
			for _, prefix := range []string{"01ZZ", "01ZY"} {
				t.Run(prefix, func(t *testing.T) {
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
							lookupPrefix := prefix
							switch scope {
							case "one-project":
								ctx = db.WithAuthorizedProjects(ctx, []string{project.UID})
							case "multiple-projects":
								ctx = db.WithAuthorizedProjects(ctx, []string{project.UID, second.UID})
							case "unrestricted":
								if prefix == "01ZY" {
									lookupPrefix = "01ZX"
								}
							}
							_, err = store.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (
    VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?
)
INSERT INTO issues(uid,short_id,project_id,title,author)
SELECT printf('01M4%022d',x),printf('%06d',x),?,'Unrelated issue','agent' FROM n`, size, project.ID)
							require.NoError(t, err)
							// The only match follows every unrelated UID in binary order.
							_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, UID: "01ZZ0000000000000000000001", Title: "Matching issue", Author: "agent"})
							require.NoError(t, err)
							return func() {
								for range 32 {
									matches, err := store.IssueUIDPrefixMatch(ctx, lookupPrefix, 1, db.IncludeDeletedNo)
									require.NoError(t, err)
									if prefix == "01ZZ" {
										require.Len(t, matches, 1)
										require.Equal(t, "01ZZ0000000000000000000001", matches[0].UID)
									} else {
										require.Empty(t, matches)
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

func TestIssueUIDPrefixMatchOrderingAndScope(t *testing.T) {
	store := openTestDB(t)
	ctx := t.Context()
	allowed, err := store.CreateProject(ctx, "allowed-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	// Reverse insertion order distinguishes UID order from insertion order.
	for _, row := range []struct {
		uid       string
		projectID int64
	}{
		{"01hznq7v000000000000000003", allowed.ID},
		{"01HZNQ7V000000000000000002", allowed.ID},
		{"01HZNQ7V000000000000000001", other.ID},
	} {
		_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: row.projectID, UID: row.uid, Title: "Prefix match", Author: "agent"})
		require.NoError(t, err)
	}
	matches, err := store.IssueUIDPrefixMatch(ctx, "01HZNQ7V", 2, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Len(t, matches, 2)
	require.Equal(t, "01HZNQ7V000000000000000001", matches[0].UID)
	require.Equal(t, "01HZNQ7V000000000000000002", matches[1].UID)

	scoped := db.WithAuthorizedProjects(ctx, []string{allowed.UID})
	matches, err = store.IssueUIDPrefixMatch(scoped, "01hznq7v", 20, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Len(t, matches, 2)
	require.Equal(t, "01HZNQ7V000000000000000002", matches[0].UID)
	require.Equal(t, "01hznq7v000000000000000003", matches[1].UID)
	matches, err = store.IssueUIDPrefixMatch(db.WithAuthorizedProjects(ctx, []string{}), "01HZNQ7V", 20, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Empty(t, matches)
}
