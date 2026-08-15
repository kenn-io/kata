package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	kataclient "go.kenn.io/kata/pkg/client"
)

func TestScopeIssueTargetRequiresQualificationInMultiProjectMode(t *testing.T) {
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}})
	require.NoError(t, err)

	_, _, err = scope.IssueTarget(t.Context(), nil, "abc4", true)
	require.EqualError(t, err, "multi-project writes require project#ref")
}

func TestScopeIssueTargetRejectsProjectOutsideAllowlist(t *testing.T) {
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}})
	require.NoError(t, err)

	_, _, err = scope.IssueTarget(t.Context(), nil, "hub-project#abc4", true)
	require.EqualError(t, err, `project "hub-project" is outside the MCP startup scope`)
}

func TestAllowlistScopeRetainsAuthorityByUIDAfterRename(t *testing.T) {
	client := projectCatalogClient(t, `{"projects":[{"id":1,"uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","name":"renamed-project","metadata":{},"revision":2,"created_at":"2026-08-11T00:00:00Z"}]}`)
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}})
	require.NoError(t, err)

	project, err := scope.Project(t.Context(), client, "renamed-project", false)
	require.NoError(t, err)
	require.Equal(t, "renamed-project", project.Name)
	require.Equal(t, "01HAAAAAAAAAAAAAAAAAAAAAAA", project.UID)
}

func TestResolveBoundScopeRetainsAuthorityByUIDAfterRename(t *testing.T) {
	client := projectCatalogClient(t, `{"projects":[
		{"id":1,"uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","name":"renamed-project","metadata":{},"revision":2,"created_at":"2026-08-11T00:00:00Z"},
		{"id":2,"uid":"01HBBBBBBBBBBBBBBBBBBBBBBB","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-08-11T00:00:00Z"}
	]}`)
	scope, err := ResolveBoundScope(t.Context(), client, ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)

	project, err := scope.Project(t.Context(), client, "", false)
	require.NoError(t, err)
	require.Equal(t, "renamed-project", project.Name)
	require.Equal(t, "01HAAAAAAAAAAAAAAAAAAAAAAA", project.UID)
}

func TestResolveAllowlistScopeRejectsDuplicateAndMissingProjects(t *testing.T) {
	client := projectCatalogClient(t, `{"projects":[{"id":1,"uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-08-11T00:00:00Z"}]}`)

	_, err := ResolveAllowlistScope(t.Context(), client, []string{"spoke-project", "spoke-project"})
	require.EqualError(t, err, `duplicate MCP project "spoke-project"`)

	_, err = ResolveAllowlistScope(t.Context(), client, []string{"missing-project"})
	require.EqualError(t, err, `MCP project "missing-project" was not found`)
}

func TestAllowlistScopeServesRemainingProjectsWhenOneIsMissing(t *testing.T) {
	client := projectCatalogClient(t, `{"projects":[{"id":1,"uid":"01HAAAAAAAAAAAAAAAAAAAAAAA","name":"spoke-project","metadata":{},"revision":1,"created_at":"2026-08-11T00:00:00Z"}]}`)
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"},
	})
	require.NoError(t, err)

	projects, err := scope.Projects(t.Context(), client, false)
	require.NoError(t, err)
	require.Len(t, projects, 1)
	require.Equal(t, "spoke-project", projects[0].Name)

	_, err = scope.Project(t.Context(), client, "spoke-project", false)
	require.NoError(t, err)
	_, err = scope.Project(t.Context(), client, "hub-project", false)
	require.EqualError(t, err, `project "hub-project" in the MCP startup scope is no longer available`)
}

func TestScopeProjectsFailWhenNoMembersRemain(t *testing.T) {
	client := projectCatalogClient(t, `{"projects":[]}`)
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}})
	require.NoError(t, err)

	_, err = scope.Projects(t.Context(), client, false)
	require.EqualError(t, err, "no projects in the MCP startup scope are available")
}

func projectCatalogClient(t *testing.T, response string) *kataclient.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/api/v1/projects", request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	return client
}
