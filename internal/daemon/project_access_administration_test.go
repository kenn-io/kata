package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R9: administrative team/policy APIs use the existing owner capability;
// members cannot administer teams or widen their own project visibility.
func TestTeamProjectAccessAdministrationAPI(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		code, _, body := f.request(t, http.MethodPost, "/api/v1/teams", "admin", map[string]any{"name": "operations"}, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		var created struct {
			Team  db.Team  `json:"team"`
			Event db.Event `json:"event"`
		}
		require.NoError(t, json.Unmarshal(body, &created))
		require.NotEmpty(t, created.Team.UID)
		require.Equal(t, "operations", created.Team.Name)
		require.Equal(t, db.BootstrapActor, created.Event.Actor)
		teamPath := "/api/v1/teams/" + created.Team.UID
		code, _, body = f.request(t, http.MethodGet, "/api/v1/teams", "admin", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		require.Contains(t, string(body), created.Team.UID)
		for _, action := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/api/v1/teams", nil},
			{http.MethodGet, teamPath, nil},
			{http.MethodPut, teamPath + "/members/nonmember", nil},
			{http.MethodDelete, teamPath + "/members/member", nil},
			{http.MethodDelete, teamPath, nil},
			{http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/access", f.private.ID), map[string]any{"visibility": "all", "team_uids": []string{}}},
		} {
			code, _, body = f.request(t, action.method, action.path, "member", action.body, nil)
			require.Equal(t, http.StatusNotFound, code, string(body))
			require.NotContains(t, string(body), created.Team.UID)
		}
		code, _, body = f.request(t, http.MethodPut, teamPath+"/members/member", "admin", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		code, _, body = f.request(t, http.MethodGet, teamPath, "admin", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		var shown struct {
			Team    db.Team  `json:"team"`
			Members []string `json:"members"`
		}
		require.NoError(t, json.Unmarshal(body, &shown))
		require.Equal(t, []string{"member"}, shown.Members)
		accessPath := fmt.Sprintf("/api/v1/projects/%d/access", f.public.ID)
		code, _, body = f.request(t, http.MethodPut, accessPath, "admin", map[string]any{"visibility": "teams", "team_uids": []string{created.Team.UID}}, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		policy, err := store.ProjectAccessPolicy(t.Context(), f.public.UID)
		require.NoError(t, err)
		require.Equal(t, "teams", policy.Visibility)
		require.Equal(t, []string{created.Team.UID}, policy.TeamUIDs)
		code, _, body = f.request(t, http.MethodGet, accessPath, "admin", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		var access struct {
			Policy db.ProjectAccessPolicy `json:"policy"`
		}
		require.NoError(t, json.Unmarshal(body, &access))
		require.Equal(t, policy, access.Policy)
		code, _, body = f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues", f.public.ID), "nonmember", nil, nil)
		require.Equal(t, http.StatusNotFound, code, string(body))
		code, _, body = f.request(t, http.MethodDelete, teamPath+"/members/member", "admin", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		code, _, body = f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues", f.public.ID), "member", nil, nil)
		require.Equal(t, http.StatusNotFound, code, string(body))
		code, _, body = f.request(t, http.MethodDelete, teamPath, "admin", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		policy, err = store.ProjectAccessPolicy(t.Context(), f.public.UID)
		require.NoError(t, err)
		require.Equal(t, "teams", policy.Visibility, "team deletion must not broaden project visibility")
		require.Empty(t, policy.TeamUIDs)
	})
}

// R9: token issuance and canonical-actor membership commit together. An
// invalid team leaves neither a token nor a partial enrollment behind.
func TestTokenInitialTeamAdministrationAPI(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		code, _, body := f.request(t, http.MethodPost, "/api/v1/tokens", "admin", map[string]any{"actor": "new-member", "team_uids": []string{f.team.UID}}, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		members, err := store.TeamMembers(t.Context(), f.team.UID)
		require.NoError(t, err)
		require.Contains(t, members, "new-member")
		before, err := store.ListAPITokens(t.Context())
		require.NoError(t, err)
		code, _, body = f.request(t, http.MethodPost, "/api/v1/tokens", "admin", map[string]any{"actor": "never-enrolled", "team_uids": []string{f.team.UID, "00000000000000000000000001"}}, nil)
		require.Equal(t, http.StatusBadRequest, code, string(body))
		after, err := store.ListAPITokens(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
		members, err = store.TeamMembers(t.Context(), f.team.UID)
		require.NoError(t, err)
		require.NotContains(t, members, "never-enrolled")
	})
}

// R9: a stale policy editor receives a conflict and cannot overwrite a newer
// visibility decision. The native revision check remains inside the mutation.
func TestProjectAccessAdministrationRevisionConflict(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		before, err := store.ProjectAccessPolicy(t.Context(), f.public.UID)
		require.NoError(t, err)
		path := fmt.Sprintf("/api/v1/projects/%d/access", f.public.ID)
		code, _, body := f.request(t, http.MethodPut, path, "admin", map[string]any{"visibility": "teams", "team_uids": []string{f.team.UID}, "revision": before.Revision}, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		current, err := store.ProjectAccessPolicy(t.Context(), f.public.UID)
		require.NoError(t, err)
		code, _, body = f.request(t, http.MethodPut, path, "admin", map[string]any{"visibility": "all", "team_uids": []string{}, "revision": before.Revision}, nil)
		require.Equal(t, http.StatusConflict, code, string(body))
		after, err := store.ProjectAccessPolicy(t.Context(), f.public.UID)
		require.NoError(t, err)
		require.Equal(t, current, after)
	})
}
