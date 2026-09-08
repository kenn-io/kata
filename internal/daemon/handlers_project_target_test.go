package daemon_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestIssueMutationsResolveProjectName(t *testing.T) {
	for _, selector := range []string{"name:example-project", "numeric"} {
		t.Run(selector, func(t *testing.T) {
			env := testenv.New(t)
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			base := "/api/v1/projects/" + selector
			if selector == "numeric" {
				base = projectPath(project.ID)
			}
			t.Run("create", func(t *testing.T) {
				resp, raw := envDoRaw(t, env, http.MethodPost, base+"/issues", map[string]any{
					"actor": "user-a", "title": "created title",
				}, nil)
				require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
				require.Equal(t, "example-project", resp.Header.Get("X-Kata-Project-Name"))
				var created struct{ Issue db.Issue }
				require.NoError(t, json.Unmarshal(raw, &created))
				require.Equal(t, project.ID, created.Issue.ProjectID)
			})
			issueID := createIssueViaHTTP(t, env, project.ID, "fixture issue")
			fixture, err := env.DB.IssueByID(t.Context(), issueID)
			require.NoError(t, err)
			path := base + "/issues/" + fixture.ShortID
			for _, mutation := range []struct {
				method, suffix string
				body           map[string]string
			}{
				{http.MethodPatch, "", map[string]string{"actor": "user-a", "title": "updated title"}},
				{http.MethodPost, "/comments", map[string]string{"actor": "user-a", "body": "comment text"}},
				{http.MethodPost, "/labels", map[string]string{"actor": "user-a", "label": "urgent"}},
			} {
				t.Run(mutation.method+mutation.suffix, func(t *testing.T) {
					resp, raw := envDoRaw(t, env, mutation.method, path+mutation.suffix, mutation.body, nil)
					require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
					require.Equal(t, "example-project", resp.Header.Get("X-Kata-Project-Name"))
				})
			}
			issue, err := env.DB.IssueByID(t.Context(), fixture.ID)
			require.NoError(t, err)
			require.Equal(t, "updated title", issue.Title)
			comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "comment text", comments[0].Body)
			labels, err := env.DB.LabelsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, labels, 1)
			require.Equal(t, "urgent", labels[0].Label)
		})
	}
}

func TestIssueMutationAliasResolution(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "original-project")
	require.NoError(t, err)
	headers := map[string]string{
		"X-Kata-Project-Alias": "example.com/team/example-project", "X-Kata-Project-Alias-Kind": "git",
	}
	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/projects/name:original-project/issues",
		map[string]string{"actor": "user-a", "title": "first issue"}, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	alias, err := env.DB.AliasByIdentity(t.Context(), headers["X-Kata-Project-Alias"])
	require.NoError(t, err)
	require.Equal(t, project.ID, alias.ProjectID)
	_, _, _, err = env.DB.RenameProjectAndEvent(t.Context(), project.ID, "renamed-project", "user-a")
	require.NoError(t, err)
	resp, raw = envDoRaw(t, env, http.MethodPost, "/api/v1/projects/name:original-project/issues",
		map[string]any{"actor": "user-a", "title": "after rename", "force_new": true}, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Equal(t, "renamed-project", resp.Header.Get("X-Kata-Project-Name"))
	target, err := env.DB.CreateProject(t.Context(), "surviving-project")
	require.NoError(t, err)
	_, err = env.DB.MergeProjects(t.Context(), db.MergeProjectsParams{SourceProjectID: project.ID, TargetProjectID: target.ID})
	require.NoError(t, err)
	resp, raw = envDoRaw(t, env, http.MethodPost, "/api/v1/projects/name:/issues",
		map[string]any{"actor": "user-a", "title": "after merge", "force_new": true}, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Equal(t, "surviving-project", resp.Header.Get("X-Kata-Project-Name"))
	var created struct{ Issue db.Issue }
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, target.ID, created.Issue.ProjectID)
}

func TestIssueMutationRejectsUnresolvedProject(t *testing.T) {
	for _, tc := range []struct {
		selector string
		status   int
	}{
		{"name:", http.StatusBadRequest},
		{"unknown", http.StatusBadRequest},
		{"name:missing-project", http.StatusNotFound},
		{"0", http.StatusNotFound},
	} {
		t.Run(tc.selector, func(t *testing.T) {
			env := testenv.New(t)
			resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/projects/"+tc.selector+"/issues",
				map[string]string{"actor": "user-a", "title": "not created"}, nil)
			require.Equal(t, tc.status, resp.StatusCode, string(raw))
			require.Empty(t, resp.Header.Get("X-Kata-Project-Name"))
		})
	}
}

func TestIssueMutationRejectsInvalidAliasTarget(t *testing.T) {
	for _, tc := range []struct {
		name, selector, identity, kind string
	}{
		{"numeric selector", "numeric", "example.com/team/example-project", "git"},
		{"missing identity", "name:example-project", "", "git"},
		{"missing kind", "name:example-project", "example.com/team/example-project", ""},
		{"invalid kind", "name:example-project", "example.com/team/example-project", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			path := "/api/v1/projects/" + tc.selector + "/issues"
			if tc.selector == "numeric" {
				path = projectPath(project.ID) + "/issues"
			}
			resp, raw := envDoRaw(t, env, http.MethodPost, path,
				map[string]string{"actor": "user-a", "title": "not created"}, map[string]string{
					"X-Kata-Project-Alias": tc.identity, "X-Kata-Project-Alias-Kind": tc.kind,
				})
			assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
			aliases, err := env.DB.ProjectAliases(t.Context(), project.ID)
			require.NoError(t, err)
			require.Empty(t, aliases)
		})
	}
}

func TestIssueMutationAliasAttachmentRespectsAuthority(t *testing.T) {
	for _, principal := range []daemon.PrincipalKind{daemon.PrincipalHost, daemon.PrincipalBootstrap, daemon.PrincipalWebLocal} {
		t.Run(string(principal), func(t *testing.T) {
			env := testenv.New(t)
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			cfg := daemon.ServerConfig{DB: env.DB}
			if principal == daemon.PrincipalHost {
				cfg.HostAccess = denyingUILaunchHostAccess{}
			}
			server := daemon.NewServer(cfg)
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: principal, Subject: "user-a", Actor: "user-a"})
				server.Handler().ServeHTTP(w, r.WithContext(ctx))
			}))
			t.Cleanup(ts.Close)
			resp := postWithHeader(t, ts, "/api/v1/projects/name:example-project/issues", map[string]string{
				"X-Kata-Project-Alias": "example.com/team/example-project", "X-Kata-Project-Alias-Kind": "git",
			}, map[string]string{"actor": "user-a", "title": "not created"})
			want := http.StatusForbidden
			if principal == daemon.PrincipalHost {
				want = http.StatusNotFound
			}
			require.Equal(t, want, resp.status, string(resp.body))
			aliases, err := env.DB.ProjectAliases(t.Context(), project.ID)
			require.NoError(t, err)
			require.Empty(t, aliases)
		})
	}
}
