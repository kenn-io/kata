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
	assert.Empty(t, audit.Operation.ProjectIDs)
	assert.True(t, audit.Operation.AllProjects)
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
		if request.Operation.ID == "showIssueByUID" && request.Operation.AllProjects {
			return kata.ErrAccessDenied
		}
		if containsInt64(request.Operation.ProjectIDs, targetProject.Project.ID) {
			switch request.Operation.ID {
			case "showIssue", "reachableIssueGraph":
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

func TestServiceAccessControllerRequiresAllProjectsBeforeUnboundedOperations(t *testing.T) {
	controller := &recordingAccessController{}
	_, server := newAccessTestServer(t, controller)
	project := createProject(t, server.URL, "example-project")
	first := createAccessIssue(t, server, project.Project.ID, "first issue")
	_ = createAccessIssue(t, server, project.Project.ID, "second issue")
	controller.authorize = func(request kata.AccessRequest) error {
		if request.Operation.AllProjects {
			return kata.ErrAccessDenied
		}
		return nil
	}

	projectID := strconv.FormatInt(project.Project.ID, 10)
	requests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/projects/" + projectID + "/issues/" + first.Issue.ShortID + "/actions/purge"},
		{http.MethodPost, "/api/v1/projects/" + projectID + "/actions/purge"},
		{http.MethodPost, "/api/v1/projects/" + projectID + "/issues/" + first.Issue.ShortID + "/actions/close"},
		{http.MethodGet, "/api/v1/projects/" + projectID + "/ready"},
		{http.MethodGet, "/api/v1/issues/" + first.Issue.UID[:8]},
		{http.MethodGet, "/api/v1/issues/01ABCDEFG"},
		{http.MethodPost, "/api/v1/projects/" + projectID + "/imports"},
		{http.MethodGet, "/api/v1/projects/" + projectID + "/events"},
		{http.MethodGet, "/api/v1/events/stream?project_id=" + projectID},
		{http.MethodGet, "/api/v1/projects/" + projectID + "/digest?since=2026-01-01T00:00:00Z"},
	}
	for _, tc := range requests {
		request, err := http.NewRequest(tc.method, server.URL+tc.path, bytes.NewBufferString(`{}`))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
		_ = response.Body.Close()
		assert.Equal(t, http.StatusNotFound, response.StatusCode, tc.path)
		assert.Equal(t, map[string]any{
			"status": float64(http.StatusNotFound),
			"error": map[string]any{
				"code": "not_found", "message": "resource not found",
			},
		}, body, tc.path)
	}

	requireAll := map[string]bool{
		"purgeIssue": true, "purgeProject": true, "closeIssue": true,
		"readyIssues": true, "showIssueByUID": true, "importIssues": true,
		"pollProjectEvents": true, "streamEvents": true, "digestProject": true,
	}
	seen := map[string]int{}
	for _, request := range controller.snapshot() {
		if !requireAll[request.Operation.ID] {
			continue
		}
		seen[request.Operation.ID]++
		assert.True(t, request.Operation.AllProjects, request.Operation.ID)
	}
	assert.Equal(t, map[string]int{
		"purgeIssue": 1, "purgeProject": 1, "closeIssue": 1,
		"readyIssues": 1, "showIssueByUID": 2, "importIssues": 1,
		"pollProjectEvents": 1, "streamEvents": 1, "digestProject": 1,
	}, seen)
}

func TestServiceAccessControllerDerivesLeaseHolderFromSubject(t *testing.T) {
	controller := &recordingAccessController{}
	service, err := kata.New(context.Background(), kata.Config{
		DSN: filepath.Join(t.TempDir(), "service.db"), Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := r.Header.Get("X-Test-Subject")
		switch subject {
		case "":
			subject = "user-one"
		case "padded":
			subject = " user-one "
		}
		principal := kata.Principal{Subject: subject, Actor: "Example User"}
		service.Handler().ServeHTTP(w, r.WithContext(kata.WithPrincipal(r.Context(), principal)))
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	project := createProject(t, server.URL, "example-project")
	issue := createAccessIssue(t, server, project.Project.ID, "leased issue")
	projectID := strconv.FormatInt(project.Project.ID, 10)
	postJSON(t, server.Client(), server.URL+"/api/v1/projects/"+projectID+"/federation/enable",
		`{"actor":"ignored"}`, nil)
	leasePath := server.URL + "/api/v1/projects/" + projectID + "/issues/" +
		issue.Issue.ShortID + "/lease/actions/"

	var acquired struct {
		Holder struct {
			Holder string `json:"holder"`
		} `json:"holder"`
	}
	postJSON(t, server.Client(), leasePath+"acquire",
		`{"holder":"caller-selected","client_kind":"cli","claim_kind":"hard"}`, &acquired)
	assert.NotEmpty(t, acquired.Holder.Holder)
	assert.NotEqual(t, "caller-selected", acquired.Holder.Holder)

	impersonation, err := http.NewRequest(http.MethodPost, leasePath+"release",
		bytes.NewBufferString(`{"holder":"`+acquired.Holder.Holder+`","client_kind":"cli"}`))
	require.NoError(t, err)
	impersonation.Header.Set("Content-Type", "application/json")
	impersonation.Header.Set("X-Test-Subject", "padded")
	impersonationResponse, err := server.Client().Do(impersonation)
	require.NoError(t, err)
	_ = impersonationResponse.Body.Close()
	assert.Equal(t, http.StatusConflict, impersonationResponse.StatusCode)

	ownerRelease, err := http.NewRequest(http.MethodPost, leasePath+"release",
		bytes.NewBufferString(`{"holder":"another-caller-value","client_kind":"cli"}`))
	require.NoError(t, err)
	ownerRelease.Header.Set("Content-Type", "application/json")
	ownerRelease.Header.Set("X-Test-Subject", "user-one")
	ownerResponse, err := server.Client().Do(ownerRelease)
	require.NoError(t, err)
	defer func() { _ = ownerResponse.Body.Close() }()
	assert.Equal(t, http.StatusOK, ownerResponse.StatusCode)
}

func TestServiceAccessControllerConcealsUnauthorizedLinkTargetState(t *testing.T) {
	controller := &recordingAccessController{}
	_, server := newAccessTestServer(t, controller)
	sourceProject := createProject(t, server.URL, "source-project")
	activeProject := createProject(t, server.URL, "active-project")
	archivedProject := createProject(t, server.URL, "archived-project")
	source := createAccessIssue(t, server, sourceProject.Project.ID, "source issue")
	active := createAccessIssue(t, server, activeProject.Project.ID, "active target")
	archived := createAccessIssue(t, server, archivedProject.Project.ID, "archived target")

	archiveRequest, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/projects/"+
		strconv.FormatInt(archivedProject.Project.ID, 10)+"?actor=ignored&force=true", nil)
	require.NoError(t, err)
	archiveResponse, err := server.Client().Do(archiveRequest)
	require.NoError(t, err)
	_ = archiveResponse.Body.Close()
	require.Equal(t, http.StatusOK, archiveResponse.StatusCode)

	controller.authorize = func(request kata.AccessRequest) error {
		if request.Operation.ID == "createLink" &&
			(containsInt64(request.Operation.ProjectIDs, activeProject.Project.ID) ||
				containsInt64(request.Operation.ProjectIDs, archivedProject.Project.ID)) {
			return kata.ErrAccessDenied
		}
		return nil
	}
	linkPath := server.URL + "/api/v1/projects/" +
		strconv.FormatInt(sourceProject.Project.ID, 10) + "/issues/" + source.Issue.ShortID + "/links"
	for _, target := range []string{active.Issue.UID, archived.Issue.UID, "01HZNQ7VFPK1XGD8R5MABCD4EZ"} {
		request, err := http.NewRequest(http.MethodPost, linkPath, bytes.NewBufferString(
			`{"actor":"ignored","type":"related","to_ref":"`+target+`"}`))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
		_ = response.Body.Close()
		assert.Equal(t, http.StatusNotFound, response.StatusCode)
		assert.Equal(t, map[string]any{
			"status": float64(http.StatusNotFound),
			"error": map[string]any{
				"code": "not_found", "message": "resource not found",
			},
		}, body)
	}
}

func TestServiceAccessControllerSuppliesFederationReplicaActor(t *testing.T) {
	controller := &recordingAccessController{}
	_, server := newAccessTestServer(t, controller)
	body := bytes.NewBufferString(`{
		"hub_url":"https://hub.example",
		"hub_project_id":42,
		"hub_project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":"replica-project",
		"replay_horizon_event_id":1,
		"actor":"forged actor"
	}`)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/federation/replicas", body)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var created struct {
		Binding struct {
			Actor string `json:"actor"`
		} `json:"binding"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&created))
	assert.Equal(t, "Example User", created.Binding.Actor)
}

func TestServiceAccessControllerAuthorizesProjectsContributingChildCounts(t *testing.T) {
	controller := &recordingAccessController{}
	_, server := newAccessTestServer(t, controller)
	parentProject := createProject(t, server.URL, "parent-project")
	childProject := createProject(t, server.URL, "child-project")
	parent := createAccessIssue(t, server, parentProject.Project.ID, "parent issue")
	child := createAccessIssue(t, server, childProject.Project.ID, "child issue")
	postJSON(t, server.Client(), server.URL+"/api/v1/projects/"+
		strconv.FormatInt(childProject.Project.ID, 10)+"/issues/"+child.Issue.ShortID+"/links",
		`{"actor":"ignored","type":"parent","to_ref":"`+parent.Issue.UID+`"}`, nil)
	controller.authorize = func(request kata.AccessRequest) error {
		if request.Operation.ID == "listIssues" &&
			containsInt64(request.Operation.ProjectIDs, childProject.Project.ID) {
			return kata.ErrAccessDenied
		}
		return nil
	}

	response, err := server.Client().Get(server.URL + "/api/v1/projects/" +
		strconv.FormatInt(parentProject.Project.ID, 10) + "/issues")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, response.StatusCode)

	requests := controller.snapshot()
	initial := operationRequest(t, requests, "listIssues", 0)
	expanded := operationRequest(t, requests, "listIssues", 1)
	assert.Equal(t, []int64{parentProject.Project.ID}, initial.Operation.ProjectIDs)
	assert.ElementsMatch(t,
		[]int64{parentProject.Project.ID, childProject.Project.ID},
		expanded.Operation.ProjectIDs)
}

func TestServiceAccessControllerDenialLeavesCrossProjectLinksUnchangedOnPurge(t *testing.T) {
	controller := &recordingAccessController{}
	_, server := newAccessTestServer(t, controller)
	sourceProject := createProject(t, server.URL, "source-project")
	targetProject := createProject(t, server.URL, "target-project")
	source := createAccessIssue(t, server, sourceProject.Project.ID, "source issue")
	target := createAccessIssue(t, server, targetProject.Project.ID, "target issue")
	postJSON(t, server.Client(), server.URL+"/api/v1/projects/"+
		strconv.FormatInt(sourceProject.Project.ID, 10)+"/issues/"+source.Issue.ShortID+"/links",
		`{"actor":"ignored","type":"related","to_ref":"`+target.Issue.UID+`"}`, nil)

	issuePath := server.URL + "/api/v1/projects/" +
		strconv.FormatInt(sourceProject.Project.ID, 10) + "/issues/" + source.Issue.ShortID
	deleteRequest, err := http.NewRequest(http.MethodPost, issuePath+"/actions/delete",
		bytes.NewBufferString(`{"actor":"ignored"}`))
	require.NoError(t, err)
	deleteRequest.Header.Set("Content-Type", "application/json")
	deleteRequest.Header.Set("X-Kata-Confirm", "DELETE source-project#"+source.Issue.ShortID)
	deleteResponse, err := server.Client().Do(deleteRequest)
	require.NoError(t, err)
	_ = deleteResponse.Body.Close()
	require.Equal(t, http.StatusOK, deleteResponse.StatusCode)

	controller.authorize = func(request kata.AccessRequest) error {
		if request.Operation.ID == "purgeIssue" && request.Operation.AllProjects {
			return kata.ErrAccessDenied
		}
		return nil
	}
	purgeRequest, err := http.NewRequest(http.MethodPost, issuePath+"/actions/purge",
		bytes.NewBufferString(`{"actor":"ignored"}`))
	require.NoError(t, err)
	purgeRequest.Header.Set("Content-Type", "application/json")
	purgeRequest.Header.Set("X-Kata-Confirm", "PURGE source-project#"+source.Issue.ShortID)
	purgeResponse, err := server.Client().Do(purgeRequest)
	require.NoError(t, err)
	_ = purgeResponse.Body.Close()
	assert.Equal(t, http.StatusNotFound, purgeResponse.StatusCode)

	showResponse, err := server.Client().Get(issuePath + "?include_deleted=true")
	require.NoError(t, err)
	defer func() { _ = showResponse.Body.Close() }()
	require.Equal(t, http.StatusOK, showResponse.StatusCode)
	var shown struct {
		Links []struct {
			To struct {
				UID string `json:"uid"`
			} `json:"to"`
		} `json:"links"`
	}
	require.NoError(t, json.NewDecoder(showResponse.Body).Decode(&shown))
	require.Len(t, shown.Links, 1)
	assert.Equal(t, target.Issue.UID, shown.Links[0].To.UID)
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
