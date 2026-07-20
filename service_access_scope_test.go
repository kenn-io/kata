package kata_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
)

type accessIssue struct {
	Issue struct {
		UID      string `json:"uid"`
		ShortID  string `json:"short_id"`
		Revision int64  `json:"revision"`
	} `json:"issue"`
}

func TestServiceAccessControllerScopesGlobalAndFilteredReads(t *testing.T) {
	controller := &recordingAccessController{}
	service, server := newAccessTestServer(t, controller)
	_ = service
	project := createProject(t, server.URL, "example-project")
	controller.authorize = func(request kata.AccessRequest) error {
		switch request.Operation.ID {
		case "listAllIssues", "auditCloses":
			return kata.ErrAccessDenied
		default:
			return nil
		}
	}

	paths := []string{
		"/api/v1/issues?project_id=" + strconv.FormatInt(project.Project.ID, 10),
		"/api/v1/issues",
		"/api/v1/audit/closes?project_id=" + strconv.FormatInt(project.Project.ID, 10),
	}
	for _, path := range paths {
		response, err := server.Client().Get(server.URL + path)
		require.NoError(t, err)
		_ = response.Body.Close()
		assert.Equal(t, http.StatusNotFound, response.StatusCode)
	}

	requests := controller.snapshot()
	filtered := operationRequest(t, requests, "listAllIssues", 0)
	global := operationRequest(t, requests, "listAllIssues", 1)
	audit := operationRequest(t, requests, "auditCloses", 0)
	assert.Equal(t, []int64{project.Project.ID}, filtered.Operation.ProjectIDs)
	assert.False(t, filtered.Operation.AllProjects)
	assert.Empty(t, global.Operation.ProjectIDs)
	assert.True(t, global.Operation.AllProjects)
	assert.Equal(t, []int64{project.Project.ID}, audit.Operation.ProjectIDs)
}

func TestServiceAccessControllerScopesFederationEnrollmentBody(t *testing.T) {
	controller := &recordingAccessController{}
	_, server := newAccessTestServer(t, controller)
	project := createProject(t, server.URL, "example-project")
	postJSON(t, server.Client(), server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/federation/enable",
		`{"actor":"ignored"}`, nil)
	controller.authorize = func(request kata.AccessRequest) error {
		if request.Operation.ID == "createFederationEnrollment" {
			return kata.ErrAccessDenied
		}
		return nil
	}

	body := bytes.NewBufferString(`{"spoke_instance_uid":"01HZNQ7VFPK1XGD8R5MABCD4EA","project_id":` +
		strconv.FormatInt(project.Project.ID, 10) +
		`,"capabilities":"pull","actor":"ignored"}`)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/federation/enrollments", body)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, response.StatusCode)

	authorized := operationRequest(t, controller.snapshot(), "createFederationEnrollment", 0)
	assert.Equal(t, []int64{project.Project.ID}, authorized.Operation.ProjectIDs)
	assert.False(t, authorized.Operation.AllProjects)
}

func TestServiceAccessControllerScopesLookupHydrationAndTraversal(t *testing.T) {
	controller := &recordingAccessController{}
	_, server := newAccessTestServer(t, controller)
	sourceProject := createProject(t, server.URL, "source-project")
	targetProject := createProject(t, server.URL, "target-project")
	source := createAccessIssue(t, server, sourceProject.Project.ID, "source issue")
	target := createAccessIssue(t, server, targetProject.Project.ID, "target issue")
	postJSON(t, server.Client(), server.URL+"/api/v1/projects/"+
		strconv.FormatInt(sourceProject.Project.ID, 10)+"/issues/"+source.Issue.ShortID+"/links",
		`{"actor":"ignored","type":"related","to_ref":"`+target.Issue.UID+`"}`, nil)

	controller.authorize = func(request kata.AccessRequest) error {
		if containsInt64(request.Operation.ProjectIDs, targetProject.Project.ID) {
			switch request.Operation.ID {
			case "showIssueByUID", "showIssue", "reachableIssueGraph":
				return kata.ErrAccessDenied
			}
		}
		return nil
	}

	paths := []string{
		"/api/v1/issues/" + source.Issue.UID,
		"/api/v1/projects/" + strconv.FormatInt(sourceProject.Project.ID, 10) +
			"/issues/" + source.Issue.ShortID,
		"/api/v1/projects/" + strconv.FormatInt(sourceProject.Project.ID, 10) +
			"/issues/" + source.Issue.ShortID + "/graph",
	}
	for _, path := range paths {
		response, err := server.Client().Get(server.URL + path)
		require.NoError(t, err)
		_ = response.Body.Close()
		assert.Equal(t, http.StatusNotFound, response.StatusCode)
	}
}

func TestServiceAccessControllerScopesCrossProjectLinkMutation(t *testing.T) {
	controller := &recordingAccessController{}
	_, server := newAccessTestServer(t, controller)
	sourceProject := createProject(t, server.URL, "source-project")
	targetProject := createProject(t, server.URL, "target-project")
	source := createAccessIssue(t, server, sourceProject.Project.ID, "source issue")
	target := createAccessIssue(t, server, targetProject.Project.ID, "target issue")
	controller.authorize = func(request kata.AccessRequest) error {
		if request.Operation.ID == "createLink" &&
			containsInt64(request.Operation.ProjectIDs, targetProject.Project.ID) {
			return kata.ErrAccessDenied
		}
		return nil
	}

	body := bytes.NewBufferString(`{"actor":"ignored","type":"related","to_ref":"` +
		target.Issue.UID + `"}`)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/projects/"+
		strconv.FormatInt(sourceProject.Project.ID, 10)+"/issues/"+source.Issue.ShortID+"/links", body)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
}

func newAccessTestServer(
	t *testing.T,
	controller *recordingAccessController,
) (*kata.Service, *httptest.Server) {
	t.Helper()
	service, err := kata.New(context.Background(), kata.Config{
		DSN: filepath.Join(t.TempDir(), "service.db"), Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	principal := kata.Principal{Subject: "user-123", Actor: "Example User"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.Handler().ServeHTTP(w, r.WithContext(kata.WithPrincipal(r.Context(), principal)))
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return service, server
}

func createAccessIssue(t *testing.T, server *httptest.Server, projectID int64, title string) accessIssue {
	t.Helper()
	body, err := json.Marshal(map[string]string{"actor": "ignored", "title": title})
	require.NoError(t, err)
	response, err := http.Post(server.URL+"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+
		"/issues", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var issue accessIssue
	require.NoError(t, json.NewDecoder(response.Body).Decode(&issue))
	return issue
}

func operationRequest(
	t *testing.T,
	requests []kata.AccessRequest,
	operationID string,
	index int,
) kata.AccessRequest {
	t.Helper()
	matching := make([]kata.AccessRequest, 0)
	for _, request := range requests {
		if request.Operation.ID == operationID {
			matching = append(matching, request)
		}
	}
	require.Greater(t, len(matching), index)
	return matching[index]
}
