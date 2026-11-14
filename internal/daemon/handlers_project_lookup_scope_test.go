package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestIssueScopedProjectLookupHidesMetadata(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = env.DB.PatchProjectMetadata(t.Context(), db.PatchProjectMetadataIn{
		ProjectID: project.ID, Actor: "owner", Patch: map[string]jsontext.Value{"private": jsontext.Value(`"hidden-value"`)},
	})
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	newScopedTokens(t, env, project, root)
	headers := map[string]string{"Authorization": "Bearer worker-token"}

	ownerResp, ownerBody := envDoRaw(t, env, http.MethodGet, "/api/v1/projects/resolve?name=example-project", nil, map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equal(t, http.StatusOK, ownerResp.StatusCode, string(ownerBody))
	require.Contains(t, string(ownerBody), "hidden-value")
	showResp, showBody := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d", project.ID), nil, headers)
	require.Equal(t, http.StatusOK, showResp.StatusCode, string(showBody))
	lookupResp, lookupBody := envDoRaw(t, env, http.MethodGet, "/api/v1/projects/resolve?name=example-project", nil, headers)
	require.Equal(t, http.StatusOK, lookupResp.StatusCode, string(lookupBody))
	var show, lookup struct {
		Project map[string]any `json:"project"`
	}
	require.NoError(t, json.Unmarshal(showBody, &show))
	require.NoError(t, json.Unmarshal(lookupBody, &lookup))
	require.Equal(t, show.Project, lookup.Project)
}

func TestReadProjectByNameHostScope(t *testing.T) {
	dbh := openTestDB(t)
	allowed, err := dbh.db.CreateProject(t.Context(), "allowed-project")
	require.NoError(t, err)
	denied, err := dbh.db.CreateProject(t.Context(), "denied-project")
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: dbh.db, StartedAt: dbh.now, HostAccess: historicalUIReferenceHostAccess{deniedProjectID: denied.ID}})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "host-user", Actor: "worker"})
		server.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)
	var deniedBody []byte
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/api/v1/projects/resolve?name=allowed-project", http.StatusOK},
		{"/api/v1/projects/resolve?name=denied-project", http.StatusNotFound},
		{"/api/v1/projects/resolve?name=missing-project", http.StatusNotFound},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+tc.path, nil)
		require.NoError(t, err)
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, tc.status, resp.StatusCode, string(body))
		if tc.status == http.StatusOK {
			require.Contains(t, string(body), allowed.Name)
		} else if deniedBody == nil {
			deniedBody = body
		} else {
			require.JSONEq(t, string(deniedBody), string(body))
		}
	}
}

func TestReviewScopedLookupDoesNotRevealOtherProjectNames(t *testing.T) {
	env := testenv.New(t)
	f := newProjectAccessFixture(t, env.DB)
	newScopedTokens(t, env, f.public, f.visible)
	for _, tc := range []struct {
		name, actor string
		headers     map[string]string
	}{
		{"issue scope", "", map[string]string{"Authorization": "Bearer worker-token"}},
		{"nonmember", "nonmember", nil},
		{"narrowed owner", "admin", map[string]string{requestProjectScopeHeader: requestScopeValue(t, f.public.UID)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, body := f.request(t, http.MethodGet, "/api/v1/projects/resolve?name="+f.public.Name, tc.actor, nil, tc.headers)
			require.Equal(t, http.StatusOK, status, string(body))
			require.Contains(t, string(body), f.public.Name)
			denied, _, deniedBody := f.request(t, http.MethodGet, "/api/v1/projects/resolve?name="+f.private.Name, tc.actor, nil, tc.headers)
			missing, _, missingBody := f.request(t, http.MethodGet, "/api/v1/projects/resolve?name=missing-project", tc.actor, nil, tc.headers)
			require.Equal(t, http.StatusNotFound, denied, string(deniedBody))
			require.Equal(t, http.StatusNotFound, missing, string(missingBody))
			require.Equal(t, string(deniedBody), string(missingBody))
		})
	}
	status, _, body := f.request(t, http.MethodGet, "/api/v1/projects/resolve?name=missing-project", "admin", nil, nil)
	require.Equal(t, http.StatusNotFound, status, string(body))
	require.Contains(t, string(body), "project_not_initialized")
}
