package sqlitestore

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/pagination"
)

func TestCreationOrderPagesUseCreatedIndex(t *testing.T) {
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	var seeded []db.Issue
	for i, stamp := range []string{
		"2026-10-01T15:00:00.123456789Z",
		"2026-10-01 09:00:00.123456789 -0700 PDT",
		"2026-10-01T16:00:00.123456789Z",
		"2026-10-01T16:00:00.123456790Z",
		"2026-10-01T15:30:00Z",
	} {
		projectID := project.ID
		if i == 4 {
			projectID = other.ID
		}
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projectID, Title: fmt.Sprintf("issue %d", i), Author: "example-agent"})
		require.NoError(t, err)
		_, err = store.ExecContext(ctx, `UPDATE issues SET created_at=? WHERE id=?`, stamp, issue.ID)
		require.NoError(t, err)
		layout := time.RFC3339Nano
		if i == 1 {
			layout = "2006-01-02 15:04:05.999999999 -0700 MST"
		}
		issue.CreatedAt, err = time.Parse(layout, stamp)
		require.NoError(t, err)
		seeded = append(seeded, issue)
	}
	for _, route := range []string{"project", "global-project", "global"} {
		for _, oldest := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oldest=%t", route, oldest), func(t *testing.T) {
				expected := slices.Clone(seeded)
				if route != "global" {
					expected = slices.DeleteFunc(expected, func(issue db.Issue) bool { return issue.ProjectID != project.ID })
				}
				slices.SortFunc(expected, func(a, b db.Issue) int {
					order := a.CreatedAt.Compare(b.CreatedAt)
					if order == 0 {
						order = cmp.Compare(a.ID, b.ID)
					}
					if !oldest {
						order = -order
					}
					return order
				})
				var after *pagination.Position
				var got []int64
				for page := 0; page <= len(expected); page++ {
					p := db.ListIssuesParams{ProjectID: project.ID, CreatedFirst: !oldest, OldestFirst: oldest, Limit: 1, After: after}
					all := db.ListAllIssuesParams{CreatedFirst: !oldest, OldestFirst: oldest, Limit: 1, After: after}
					if route == "global-project" {
						all.ProjectID = project.ID
					}
					query, args := buildListIssuesQuery(ctx, p, false)
					if route != "project" {
						query, args = buildListAllIssuesQuery(ctx, all, false)
					}
					rows, err := store.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
					require.NoError(t, err)
					var details []string
					for rows.Next() {
						var id, parent, unused int
						var detail string
						require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
						details = append(details, detail)
					}
					require.NoError(t, rows.Err())
					require.NoError(t, rows.Close())
					plan := strings.Join(details, "\n")
					index := "idx_issues_project_created"
					if route == "global" {
						index = "idx_issues_created"
					}
					require.Contains(t, plan, index)
					require.NotContains(t, plan, "TEMP B-TREE")
					if after != nil {
						seek := "<expr><?"
						if oldest {
							seek = "<expr>>?"
						}
						require.Contains(t, plan, seek)
					}
					var issues []db.Issue
					if route == "project" {
						issues, err = store.ListIssues(ctx, p)
					} else {
						issues, err = store.ListAllIssues(ctx, all)
					}
					require.NoError(t, err)
					if len(issues) == 0 {
						break
					}
					require.Len(t, issues, 1)
					got = append(got, issues[0].ID)
					after = &pagination.Position{CreatedAt: issues[0].CreatedAt, ID: issues[0].ID}
				}
				want := make([]int64, len(expected))
				for i, issue := range expected {
					want[i] = issue.ID
				}
				require.Equal(t, want, got)
			})
		}
	}
}
