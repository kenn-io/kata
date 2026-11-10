package sqlitestore_test

import (
	"encoding/json/jsontext"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/metadata"
)

func TestScheduleProjectionRequiresFeature(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(t.Context(), "project")
	require.NoError(t, err)
	value, err := metadata.NewScheduleValue("2026-05-18", "UTC")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "scheduled", Author: "worker", Metadata: map[string]jsontext.Value{"scheduled_on": value}})
	require.NoError(t, err)
	required, err := db.ProjectRequiredEventFeatures(t.Context(), store, project.UID)
	require.NoError(t, err)
	require.Contains(t, required, db.IssueScheduleEventFeature)
}
