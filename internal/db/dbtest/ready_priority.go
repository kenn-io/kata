package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkReadyPriorityOrder(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	primary, err := store.CreateProject(ctx, "primary-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	for _, global := range []bool{false, true} {
		name := "project"
		if global {
			name = "global"
		}
		t.Run(name, func(t *testing.T) {
			// Restrict each run to its own fixtures while spanning two projects globally.
			filter := db.ReadyIssuesFilter{PriorityFirst: true}
			create := func(projectID int64, title string, priority *int64, metadata map[string]jsontext.Value) db.Issue {
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: projectID, Title: title, Author: "author", Priority: priority, Metadata: metadata,
				})
				require.NoError(t, err)
				filter.AllowedIssueIDs = append(filter.AllowedIssueIDs, issue.ID)
				return issue
			}
			for range 9 {
				create(primary.ID, "scheduled P0", new(int64(0)), map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2099-01-01"`)})
			}
			filter.At = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
			best := create(primary.ID, "due P1", new(int64(1)), nil)
			newer := create(primary.ID, "newer P2", new(int64(2)), nil)
			secondProject := primary.ID
			if global {
				secondProject = other.ID
			}
			older := create(secondProject, "older P2", new(int64(2)), nil)
			unprioritized := create(secondProject, "newest unprioritized", nil, nil)
			sqlStore := store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			// Seed distinct times so ordering never depends on clock precision.
			for i, issue := range []db.Issue{older, newer, unprioritized} {
				updatedAt := filter.At.Add(time.Duration(i) * time.Minute)
				_, err := sqlStore.ExecContext(ctx, `UPDATE issues SET updated_at=$1 WHERE id=$2`, updatedAt.Format(time.RFC3339Nano), issue.ID)
				require.NoError(t, err)
			}
			for _, limit := range []int{1, 0} {
				var issues []db.Issue
				if global {
					rows, err := store.ReadyIssuesGlobal(ctx, limit, filter)
					require.NoError(t, err)
					for _, row := range rows {
						issues = append(issues, row.Issue)
					}
				} else {
					issues, err = store.ReadyIssues(ctx, primary.ID, limit, filter)
					require.NoError(t, err)
				}
				var ids []int64
				for _, issue := range issues {
					ids = append(ids, issue.ID)
				}
				want := []int64{best.ID}
				if limit == 0 {
					want = []int64{best.ID, newer.ID, older.ID, unprioritized.ID}
				}
				require.Equal(t, want, ids)
			}
		})
	}
	return nil
}
