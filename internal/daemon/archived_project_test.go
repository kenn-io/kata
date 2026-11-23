package daemon

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type denyProjectHost struct{ deniedID int64 }

func (h denyProjectHost) Authorize(_ context.Context, request HostAccessRequest) (HostAccessDecision, error) {
	if slices.Contains(request.Operation.ProjectIDs, h.deniedID) {
		return HostAccessDecision{}, ErrHostAccessDenied
	}
	return HostAccessDecision{}, nil
}

func TestArchivedProjectHintsRestoreOnlyForCallersWhoMayReadIt(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	archived, err := store.CreateProject(t.Context(), "retired-project")
	require.NoError(t, err)
	other, err := store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	scopedTo := func(project db.Project) context.Context {
		return WithPrincipal(t.Context(), Principal{
			Kind: PrincipalDBToken, Actor: "worker-a",
			Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: "root"},
		})
	}
	_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: archived.ID, Actor: "admin", Force: true})
	require.NoError(t, err)

	hosted := func(controller HostAccessController) context.Context {
		return context.WithValue(t.Context(), hostAccessStateContextKey{},
			&hostAccessState{controller: controller, request: HostAccessRequest{Operation: HostOperation{ID: "showProject"}}})
	}
	withProjectPolicy := context.WithValue(t.Context(), projectAccessContextKey{},
		&ProjectAccessDecision{ProjectUIDs: []string{other.UID}})
	tests := []struct {
		name     string
		ctx      context.Context
		id       int64
		archived bool
	}{
		{"unrestricted caller", t.Context(), archived.ID, true},
		{"issue token of the archived project", scopedTo(archived), archived.ID, false},
		{"host that allows the project", hosted(denyProjectHost{deniedID: other.ID}), archived.ID, true},
		{"host that denies the project", hosted(denyProjectHost{deniedID: archived.ID}), archived.ID, false},
		{"project policy that excludes the project", withProjectPolicy, archived.ID, false},
		{"missing project", t.Context(), archived.ID + 1000, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := activeProjectByID(tc.ctx, store, tc.id)
			apiErr, ok := err.(*api.APIError)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, 404, apiErr.Status)
			assert.Equal(t, "project_not_found", apiErr.Code)
			if tc.archived {
				assert.Equal(t, "project retired-project is archived; run 'kata projects restore retired-project' to bring it back", apiErr.Hint)
				return
			}
			assert.Empty(t, apiErr.Hint)
		})
	}
	for _, name := range []string{"", "retired-project"} {
		_, err = resolveByAliasInput(withProjectPolicy, store, &api.AliasInput{Identity: "github.com/example/retired-project", Kind: "git"}, name, false)
		apiErr, ok := err.(*api.APIError)
		require.True(t, ok, "got %v", err)
		hint := `run "kata init" in this workspace`
		if name != "" {
			hint = `run "kata init" with --project set to "retired-project" in this workspace`
		}
		assert.Equal(t, hint, apiErr.Hint)
	}
}
