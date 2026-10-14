package daemon_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// Project administration follows project access: a principal that can see a
// project may administer it, and one that cannot gets the same 404 as for a
// missing project, with nothing changed.
func TestProjectAccessMemberAdministration(t *testing.T) {
	for _, action := range []string{"purge-issue", "rename", "archive", "rewrite-author"} {
		t.Run(action, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newProjectAccessFixture(t, store)
				request := func(actor string) (int, []byte) {
					project := fmt.Sprintf("/api/v1/projects/%d", f.private.ID)
					switch action {
					case "purge-issue":
						status, _, body := f.request(t, http.MethodPost,
							project+"/issues/"+f.issue.ShortID+"/actions/purge", actor,
							map[string]string{"actor": actor},
							map[string]string{"X-Kata-Confirm": "PURGE " + f.private.Name + "#" + f.issue.ShortID})
						return status, body
					case "rename":
						status, _, body := f.request(t, http.MethodPatch, project, actor,
							map[string]string{"name": "renamed-project", "actor": actor}, nil)
						return status, body
					case "archive":
						status, _, body := f.request(t, http.MethodDelete,
							project+"?force=true&actor="+actor, actor, nil, nil)
						return status, body
					default:
						status, _, body := f.request(t, http.MethodPost, project+"/actions/rewrite-author",
							actor, map[string]string{"actor": actor, "from": "member", "to": "renamed-author"}, nil)
						return status, body
					}
				}

				status, body := request("nonmember")
				require.Equal(t, http.StatusNotFound, status, "a nonmember cannot administer: %s", body)
				assert.NotContains(t, string(body), f.private.UID)
				issue, err := store.IssueByUID(t.Context(), f.issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err, "denial must preserve the issue")
				assert.Equal(t, "member", issue.Author)
				project, err := store.ProjectByID(t.Context(), f.private.ID)
				require.NoError(t, err)
				assert.Equal(t, f.private.Name, project.Name)
				assert.Nil(t, project.DeletedAt)

				status, body = request("member")
				require.Equal(t, http.StatusOK, status, "a member administers its project: %s", body)
				project, err = store.ProjectByID(t.Context(), f.private.ID)
				require.NoError(t, err)
				issue, err = store.IssueByUID(t.Context(), f.issue.UID, db.IncludeDeletedYes)
				switch action {
				case "purge-issue":
					assert.ErrorIs(t, err, db.ErrNotFound)
				case "rename":
					assert.Equal(t, "renamed-project", project.Name)
				case "archive":
					assert.NotNil(t, project.DeletedAt)
				default:
					require.NoError(t, err)
					assert.Equal(t, "renamed-author", issue.Author)
				}
			})
		})
	}
}

// An archived project stays visible and restorable to principals that can
// access it. Purge and restore are fenced to that access.
func TestProjectAccessArchivedProjectAdministration(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, _, err := store.RemoveProject(t.Context(), db.RemoveProjectParams{
			ProjectID: f.private.ID, Actor: "admin", Force: true,
		})
		require.NoError(t, err)

		status, _, body := f.request(t, http.MethodGet, "/api/v1/projects?include=archived", "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		assert.Contains(t, string(body), f.private.UID, "members see archived projects they can access")
		status, _, body = f.request(t, http.MethodGet, "/api/v1/projects?include=archived", "nonmember", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		assert.NotContains(t, string(body), f.private.UID)
		assert.Contains(t, string(body), f.public.UID)

		restore := fmt.Sprintf("/api/v1/projects/%d/restore?actor=", f.private.ID)
		status, _, body = f.request(t, http.MethodPost, restore+"nonmember", "nonmember", nil, nil)
		require.Equal(t, http.StatusNotFound, status, string(body))
		project, err := store.ProjectByID(t.Context(), f.private.ID)
		require.NoError(t, err)
		require.NotNil(t, project.DeletedAt, "denied restore leaves the project archived")
		status, _, body = f.request(t, http.MethodPost, restore+"member", "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		project, err = store.ProjectByID(t.Context(), f.private.ID)
		require.NoError(t, err)
		require.Nil(t, project.DeletedAt)

		_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{
			ProjectID: f.private.ID, Actor: "admin", Force: true,
		})
		require.NoError(t, err)
		purge := fmt.Sprintf("/api/v1/projects/%d/actions/purge", f.private.ID)
		confirm := map[string]string{"X-Kata-Confirm": "PURGE " + f.private.Name}
		status, _, body = f.request(t, http.MethodPost, purge, "nonmember",
			map[string]string{"actor": "nonmember"}, confirm)
		require.Equal(t, http.StatusNotFound, status, string(body))
		_, err = store.ProjectByID(t.Context(), f.private.ID)
		require.NoError(t, err, "denied purge keeps the project")
		status, _, body = f.request(t, http.MethodPost, purge, "member",
			map[string]string{"actor": "member"}, confirm)
		require.Equal(t, http.StatusOK, status, string(body))
		_, err = store.ProjectByID(t.Context(), f.private.ID)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})
}

// Merge requires access to both the source and the target project.
func TestProjectAccessMergeFencesBothProjects(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		merge := func(actor string, source, target db.Project) (int, []byte) {
			status, _, body := f.request(t, http.MethodPost,
				fmt.Sprintf("/api/v1/projects/%d/merge", target.ID), actor,
				map[string]any{"source_project_id": source.ID, "actor": actor}, nil)
			return status, body
		}
		for _, pair := range [][2]db.Project{{f.private, f.public}, {f.public, f.private}} {
			status, body := merge("nonmember", pair[0], pair[1])
			require.Equal(t, http.StatusNotFound, status, string(body))
			assert.NotContains(t, string(body), f.private.UID)
		}
		for _, project := range []db.Project{f.private, f.public} {
			current, err := store.ProjectByID(t.Context(), project.ID)
			require.NoError(t, err)
			assert.Nil(t, current.DeletedAt, "denied merge leaves %s active", project.Name)
		}

		status, body := merge("member", f.public, f.private)
		require.Equal(t, http.StatusOK, status, string(body))
		moved, err := store.IssueByID(t.Context(), f.visible.ID)
		require.NoError(t, err)
		assert.Equal(t, f.private.ID, moved.ProjectID)
	})
}

// Alias detach and issue-sync administration follow project access.
func TestProjectAccessAliasAndIssueSyncAdministration(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		alias, err := store.AttachAlias(t.Context(), f.private.ID, "git:example.com/private-project", "git")
		require.NoError(t, err)
		status := fmt.Sprintf("/api/v1/projects/%d/issue-sync/github/status", f.private.ID)
		code, _, body := f.request(t, http.MethodGet, status, "nonmember", nil, nil)
		require.Equal(t, http.StatusNotFound, code, string(body))
		code, _, body = f.request(t, http.MethodGet, status, "member", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		assert.Contains(t, string(body), "not_enabled")

		// With no binding and no repository, every sync mutation fails validation
		// once it reaches its handler. A nonmember never reaches it.
		for _, action := range []string{"enable", "disable", "once"} {
			path := fmt.Sprintf("/api/v1/projects/%d/issue-sync/github/%s", f.private.ID, action)
			code, _, body = f.request(t, http.MethodPost, path, "nonmember", map[string]any{"actor": "nonmember"}, nil)
			assert.Equal(t, http.StatusNotFound, code, "%s: %s", action, body)
			assert.NotContains(t, string(body), "validation")
			code, _, body = f.request(t, http.MethodPost, path, "member", map[string]any{"actor": "member"}, nil)
			assert.Equal(t, http.StatusBadRequest, code, "%s: %s", action, body)
			assert.Contains(t, string(body), `"validation"`)
		}
		_, err = store.IssueSyncBindingByProject(t.Context(), f.private.ID)
		assert.ErrorIs(t, err, db.ErrNotFound, "no sync binding was created")

		detach := fmt.Sprintf("/api/v1/projects/%d/aliases/%d?force=true&actor=", f.private.ID, alias.ID)
		code, _, body = f.request(t, http.MethodDelete, detach+"nonmember", "nonmember", nil, nil)
		require.Equal(t, http.StatusNotFound, code, string(body))
		_, err = store.AliasByIdentity(t.Context(), "git:example.com/private-project")
		require.NoError(t, err, "denied detach keeps the alias")
		code, _, body = f.request(t, http.MethodDelete, detach+"member", "member", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		_, err = store.AliasByIdentity(t.Context(), "git:example.com/private-project")
		assert.ErrorIs(t, err, db.ErrNotFound)
	})
}

// Token administration stays with the daemon owner. A member that could mint
// a token for another actor would inherit that actor's team access.
func TestProjectAccessTokenAdministrationRequiresOwner(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		tokens, err := store.ListAPITokens(t.Context())
		require.NoError(t, err)
		require.NotEmpty(t, tokens)
		for _, actor := range []string{"member", "nonmember"} {
			code, _, body := f.request(t, http.MethodGet, "/api/v1/tokens", actor, nil, nil)
			assert.Equal(t, http.StatusNotFound, code, string(body))
			code, _, body = f.request(t, http.MethodPost, "/api/v1/tokens", actor,
				map[string]string{"actor": "other-member"}, nil)
			assert.Equal(t, http.StatusNotFound, code, string(body))
			code, _, body = f.request(t, http.MethodPost,
				fmt.Sprintf("/api/v1/tokens/%d/revoke", tokens[0].ID), actor, map[string]any{}, nil)
			assert.Equal(t, http.StatusNotFound, code, string(body))
		}
		after, err := store.ListAPITokens(t.Context())
		require.NoError(t, err)
		assert.Len(t, after, len(tokens), "denied creation adds no token")
		for _, token := range after {
			assert.Nil(t, token.RevokedAt, "denied revocation keeps %s active", token.Actor)
		}

		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token"}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		ownerHTTP := httptest.NewServer(server.Handler())
		t.Cleanup(ownerHTTP.Close)
		f.server = ownerHTTP
		code, _, body := f.request(t, http.MethodGet, "/api/v1/tokens", "admin", nil, nil)
		require.Equal(t, http.StatusOK, code, "the owner keeps token administration: %s", body)
	})
}

// The daemon switcher hides credentialed targets only from a principal that
// project policy actually limits. A team member who can see every project
// keeps the remote daemons it had before teams existed.
func TestProjectAccessGatewayAllowsPrincipalWithNoHiddenProjects(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		snapshots := 0
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/ui/snapshot" {
				snapshots++
			}
			_, _ = w.Write([]byte(projectAccessCanary))
		}))
		t.Cleanup(remote.Close)
		server := daemon.NewServer(daemon.ServerConfig{
			DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true},
			WebDaemons: []config.CatalogDaemonConfig{{Name: "remote-example", URL: remote.URL, Token: "remote-test-token"}},
		})
		t.Cleanup(func() { _ = server.Close() })
		gateway := httptest.NewServer(server.Handler())
		t.Cleanup(gateway.Close)
		f.server = gateway

		status, _, body := f.request(t, http.MethodGet, "/api/v1/ui/daemons", "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		assert.Contains(t, string(body), "remote-example")
		status, _, body = f.request(t, http.MethodGet, "/api/v1/ui/proxy/api/v1/ui/snapshot", "member", nil,
			map[string]string{"X-Kata-Web-Daemon": "remote-example"})
		assert.NotEqual(t, http.StatusForbidden, status, string(body))
		assert.Equal(t, 1, snapshots, "the gateway forwards the member's read")

		status, _, body = f.request(t, http.MethodGet, "/api/v1/ui/daemons", "nonmember", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		assert.NotContains(t, string(body), "remote-example", "a principal with hidden projects keeps the restriction")
		status, _, body = f.request(t, http.MethodGet, "/api/v1/ui/proxy/api/v1/ui/snapshot", "nonmember", nil,
			map[string]string{"X-Kata-Web-Daemon": "remote-example"})
		assert.Equal(t, http.StatusForbidden, status, string(body))
		assert.Equal(t, 1, snapshots, "the restricted read never reaches the target")
	})
}
