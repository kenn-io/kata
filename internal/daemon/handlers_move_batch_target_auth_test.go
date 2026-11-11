package daemon_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestMoveBatchTargetVisibility(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := newProjectAccessFixture(t, store)
	archived, err := store.CreateProject(t.Context(), "archived-target")
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: archived.UID, Visibility: "teams", TeamUIDs: []string{f.team.UID}}, "admin")
	require.NoError(t, err)
	_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: archived.ID, Actor: "admin"})
	require.NoError(t, err)
	target, err := store.CreateProject(t.Context(), "allowed-target")
	require.NoError(t, err)
	body := map[string]any{"items": []map[string]any{
		{"ref": f.visible.UID, "to_project_uid": f.private.UID},
		{"ref": f.visible.UID, "to_project_uid": "missing-project"},
		{"ref": f.visible.UID, "to_project_uid": archived.UID},
		{"ref": f.visible.UID, "to_project_uid": target.UID},
	}}
	status, _, raw := f.request(t, http.MethodPost, "/api/v1/issues/actions/move", "nonmember", body, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var result batchReply
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result.Results, 4)
	require.Equal(t, "not_found", result.Results[0].Code)
	require.Equal(t, "not_found", result.Results[1].Code)
	require.Equal(t, "not_found", result.Results[2].Code)
	require.Equal(t, "moved", result.Results[3].Result)
	moved, err := store.IssueByID(t.Context(), f.visible.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, moved.ProjectID)
}
