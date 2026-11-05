package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/growthtest"
)

// Allocations guard against fetching the whole ready queue. Each store's
// TestReadyPriorityUsesOrderedIndexes covers SQL scans and sorts.
func TestNextReadyGrowth(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "project"
		if all {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			growthtest.Run(t, growthtest.Check{
				Small: 1000, Large: 10000,
				MaxRatio: 2, Floor: 100,
				Measure: growthtest.Allocs(5),
				Build: func(tb testing.TB, size int) (func(), func()) {
					env, dir, projectID := setupCLIWorkspace(t)
					_, err := env.DB.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (
    VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?
)
INSERT INTO issues(uid,project_id,short_id,title,author,priority,updated_at)
SELECT printf('%026d',x),?,printf('%026d',x),'ordinary ready issue','tester',3,'2026-01-02T00:00:00Z'
FROM n`, size-1, projectID)
					require.NoError(tb, err)
					_, err = env.DB.ExecContext(t.Context(), `INSERT INTO issues(uid,project_id,short_id,title,author,priority,updated_at)
VALUES('01AAAAAAAAAAAAAAAAAAAAAAAA',?,'aaaaaaaa','highest priority winner','tester',0,'2026-01-01T00:00:00Z')`, projectID)
					require.NoError(tb, err)
					args := []string{"next", "--json"}
					if all {
						args = append(args, "--all")
					}
					return func() {
						var out string
						if all {
							out = requireCmdOutput(t, env, args...)
						} else {
							out = runCLI(t, env, dir, args...)
						}
						var got struct {
							Issue struct {
								UID   string `json:"uid"`
								Title string `json:"title"`
							} `json:"issue"`
						}
						require.NoError(tb, json.Unmarshal([]byte(out), &got))
						require.Equal(tb, "01AAAAAAAAAAAAAAAAAAAAAAAA", got.Issue.UID)
						require.Equal(tb, "highest priority winner", got.Issue.Title)
					}, nil
				},
			})
		})
	}
}
