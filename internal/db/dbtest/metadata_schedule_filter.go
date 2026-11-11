package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// RunScheduleMetadataFilter checks the existing equality filter against saved dates.
func RunScheduleMetadataFilter(t *testing.T, open func(*testing.T) db.Storage) {
	store := open(t)
	project, err := store.CreateProject(t.Context(), "schedule-filter")
	require.NoError(t, err)
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		t.Run(field, func(t *testing.T) {
			saved, err := metadata.NewScheduleValue("2026-11-12", "America/New_York")
			require.NoError(t, err)
			var want []string
			for _, raw := range []jsontext.Value{saved, jsontext.Value(`"2026-11-12"`)} {
				issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "scheduled", Author: "user-a", Metadata: map[string]jsontext.Value{field: raw}})
				require.NoError(t, err)
				want = append(want, issue.UID)
			}
			opaque, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "opaque", Author: "user-a"})
			require.NoError(t, err)
			executor := store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			_, err = executor.ExecContext(t.Context(), `UPDATE issues SET metadata = $1 WHERE id = $2`, fmt.Sprintf(`{"%s":{"value":"2026-11-12","legacy":true}}`, field), opaque.ID)
			require.NoError(t, err)
			filter := []db.MetaFilter{{Key: field, Value: "2026-11-12", HasValue: true}}
			projectQuery := db.ListIssuesParams{ProjectID: project.ID, Meta: filter}
			globalQuery := db.ListAllIssuesParams{AllowedProjectIDs: []int64{project.ID}, Meta: filter}
			projectRows, err := store.ListIssues(t.Context(), projectQuery)
			require.NoError(t, err)
			globalRows, err := store.ListAllIssues(t.Context(), globalQuery)
			require.NoError(t, err)
			projectCount, err := store.CountIssues(t.Context(), projectQuery)
			require.NoError(t, err)
			globalCount, err := store.CountAllIssues(t.Context(), globalQuery)
			require.NoError(t, err)
			var projectIDs, globalIDs []string
			for _, issue := range projectRows {
				projectIDs = append(projectIDs, issue.UID)
			}
			for _, issue := range globalRows {
				globalIDs = append(globalIDs, issue.UID)
			}
			assert.ElementsMatch(t, want, projectIDs)
			assert.ElementsMatch(t, want, globalIDs)
			assert.Equal(t, int64(2), projectCount)
			assert.Equal(t, int64(2), globalCount)
			presence, err := store.CountIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID, Meta: []db.MetaFilter{{Key: field}}})
			require.NoError(t, err)
			assert.Equal(t, int64(3), presence)
			_, err = executor.ExecContext(t.Context(), `UPDATE issues SET metadata = $1 WHERE id = $2`, fmt.Sprintf(`{"%s":{"value":123,"timezone":"UTC","tzdata_version":"v"}}`, field), opaque.ID)
			require.NoError(t, err)
			numeric, err := store.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID, Meta: []db.MetaFilter{{Key: field, Value: "123", HasValue: true}}})
			require.NoError(t, err)
			assert.Empty(t, numeric)
		})
	}
	ordinary, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "ordinary", Author: "user-a", Metadata: map[string]jsontext.Value{"work.branch": jsontext.Value(`"main"`), "custom": jsontext.Value(`{"value":"main","timezone":"UTC","tzdata_version":"v"}`)}})
	require.NoError(t, err)
	rows, err := store.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID, Meta: []db.MetaFilter{{Key: "work.branch", Value: "main", HasValue: true}}})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, ordinary.UID, rows[0].UID)
	rows, err = store.ListAllIssues(t.Context(), db.ListAllIssuesParams{AllowedProjectIDs: []int64{project.ID}, Meta: []db.MetaFilter{{Key: "custom", Value: "main", HasValue: true}}})
	require.NoError(t, err)
	require.Empty(t, rows)
}
