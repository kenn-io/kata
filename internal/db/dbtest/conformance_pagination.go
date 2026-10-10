package dbtest

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/pagination"
)

// Creation keysets must survive ordinary edits, including timestamp ties.
func checkListPagination(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := make([]db.ImportItem, 6)
	for i := range items {
		items[i] = db.ImportItem{ExternalID: fmt.Sprint(i), Title: fmt.Sprintf("matching issue %d", i), Author: "example-agent", Status: "open", CreatedAt: base, UpdatedAt: base, Labels: []string{"tracked"}}
		if i > 0 {
			items[i].Priority = new(int64(i - 1))
		}
	}
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "fixture", Actor: "example-agent", Items: items})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "foreign", Author: "example-agent"})
	require.NoError(t, err)
	original, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID, OldestFirst: true})
	require.NoError(t, err)
	require.Len(t, original, 6)
	for _, global := range []bool{false, true} {
		for _, oldest := range []bool{false, true} {
			var after *pagination.Position
			seen := map[int64]bool{}
			for pageIndex := range 7 {
				var page []db.Issue
				if global {
					page, err = store.ListAllIssues(ctx, db.ListAllIssuesParams{AllowedProjectIDs: []int64{p.ID}, OldestFirst: oldest, CreatedFirst: true, After: after, Limit: 1})
				} else {
					page, err = store.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID, OldestFirst: oldest, CreatedFirst: true, After: after, Limit: 1})
				}
				require.NoError(t, err)
				if len(page) == 0 {
					break
				}
				require.Len(t, page, 1)
				require.False(t, seen[page[0].ID])
				seen[page[0].ID] = true
				after = &pagination.Position{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
				for _, issue := range original {
					_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Body: new(fmt.Sprintf("updated body %d", pageIndex)), Actor: "example-agent"})
					require.NoError(t, err)
				}
			}
			require.Len(t, seen, 6)
		}
	}
	var partitions int64
	for priority := -1; priority < 5; priority++ {
		p1 := db.ListIssuesParams{ProjectID: p.ID, PriorityUnset: priority < 0}
		p2 := db.ListAllIssuesParams{AllowedProjectIDs: []int64{p.ID}, PriorityUnset: priority < 0}
		if priority >= 0 {
			p1.Priority = new(int64(priority))
			p2.Priority = new(int64(priority))
		}
		a, err := store.CountIssues(ctx, p1)
		require.NoError(t, err)
		require.Equal(t, int64(1), a)
		b, err := store.CountAllIssues(ctx, p2)
		require.NoError(t, err)
		require.Equal(t, a, b)
		rows, err := store.ListIssues(ctx, p1)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		partitions += a
	}
	count, err := store.CountIssues(ctx, db.ListIssuesParams{ProjectID: p.ID, Limit: 1, After: &pagination.Position{CreatedAt: base.Add(time.Hour), ID: 999}})
	require.NoError(t, err)
	require.Equal(t, partitions, count)
	count, err = store.CountIssues(ctx, db.ListIssuesParams{ProjectID: p.ID, MaxPriority: new(int64(4))})
	require.NoError(t, err)
	require.Equal(t, int64(5), count)
	count, err = store.CountAllIssues(ctx, db.ListAllIssuesParams{AllowedProjectIDs: []int64{}, Labels: []string{"tracked"}})
	require.NoError(t, err)
	require.Zero(t, count)
	count, err = store.CountIssues(ctx, db.ListIssuesParams{ProjectID: p.ID, AllowedIssueIDs: []int64{original[0].ID}, Labels: []string{"tracked"}})
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	return nil
}

func checkSearchPagination(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := make([]db.ImportItem, 205)
	for i := range items {
		items[i] = db.ImportItem{ExternalID: fmt.Sprint(i), Title: "pagination needle", Author: "example-agent", Status: "open", CreatedAt: base, UpdatedAt: base}
	}
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: "fixture", Actor: "example-agent", Items: items})
	require.NoError(t, err)
	for _, anyMatch := range []bool{false, true} {
		params := db.SearchFTSParams{ProjectID: p.ID, Query: "needle", StableOrder: true, Limit: 201}
		search := store.SearchFTS
		if anyMatch {
			search = store.SearchFTSAny
		}
		first, err := search(ctx, params)
		require.NoError(t, err)
		require.Len(t, first, 201)
		params.After = &pagination.Position{CreatedAt: first[200].Issue.CreatedAt, ID: first[200].Issue.ID}
		second, err := search(ctx, params)
		require.NoError(t, err)
		require.Len(t, second, 4)
		seen := map[int64]bool{}
		for _, hit := range append(first, second...) {
			require.False(t, seen[hit.Issue.ID])
			seen[hit.Issue.ID] = true
		}
		require.Len(t, seen, 205)
	}
	return nil
}

// The page key must use the timestamp actually stored by each backend, even
// when two distinct creation instants fall inside one millisecond.
func checkCreationPaginationPrecision(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	base := time.Date(2026, 1, 1, 23, 59, 59, 999000000, time.UTC)
	items := make([]db.ImportItem, 3)
	for i := range items {
		stamp := base.Add(time.Duration(3-i) * time.Microsecond)
		if i == 0 {
			stamp = base.Add(time.Second)
		}
		items[i] = db.ImportItem{ExternalID: fmt.Sprint(i), Title: "precision needle", Author: "example-agent", Status: "open", CreatedAt: stamp, UpdatedAt: stamp}
	}
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{ProjectID: project.ID, Source: "fixture", Actor: "example-agent", Items: items})
	require.NoError(t, err)
	want, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	slices.SortFunc(want, func(a, b db.Issue) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	for _, global := range []bool{false, true} {
		var after *pagination.Position
		for _, expected := range want {
			params := db.ListIssuesParams{ProjectID: project.ID, OldestFirst: true, Limit: 1, After: after}
			got, err := store.ListIssues(ctx, params)
			if global {
				got, err = store.ListAllIssues(ctx, db.ListAllIssuesParams{ProjectID: project.ID, OldestFirst: true, Limit: 1, After: after})
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, expected.ID, got[0].ID)
			after = &pagination.Position{CreatedAt: got[0].CreatedAt, ID: got[0].ID}
		}
	}
	var after *pagination.Position
	for _, expected := range want {
		got, err := store.SearchFTS(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: "needle", StableOrder: true, Limit: 1, After: after})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, expected.ID, got[0].Issue.ID)
		after = &pagination.Position{CreatedAt: got[0].Issue.CreatedAt, ID: got[0].Issue.ID}
	}
	return nil
}
