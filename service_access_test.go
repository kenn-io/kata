package kata_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
)

type recordingAccessController struct {
	mu        sync.Mutex
	requests  []kata.AccessRequest
	err       error
	lease     kata.AccessLease
	authorize func(kata.AccessRequest) error
}

func (c *recordingAccessController) Authorize(
	_ context.Context,
	request kata.AccessRequest,
) (kata.AccessDecision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	decision := kata.AccessDecision{}
	if request.Operation.ID == "streamEvents" {
		decision.Lease = c.lease
	}
	if c.authorize != nil {
		return decision, c.authorize(request)
	}
	return decision, c.err
}

type revocableAccessLease struct {
	revoked atomic.Bool
}

func (l *revocableAccessLease) Revalidate(context.Context) error {
	if l.revoked.Load() {
		return kata.ErrAccessDenied
	}
	return nil
}

func (c *recordingAccessController) snapshot() []kata.AccessRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]kata.AccessRequest(nil), c.requests...)
}

func TestServiceAccessControllerRequiresAnInProcessPrincipal(t *testing.T) {
	controller := &recordingAccessController{}
	service, err := kata.New(context.Background(), kata.Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))

	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.JSONEq(t, `{"status":401,"error":{"code":"authentication_required","message":"authentication required"}}`,
		response.Body.String())
	assert.Empty(t, controller.snapshot(), "authorization must not run without an authenticated principal")
}

func TestServiceAccessControllerDeniesWithoutDisclosingProjectData(t *testing.T) {
	controller := &recordingAccessController{err: kata.ErrAccessDenied}
	service, err := kata.New(context.Background(), kata.Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/42", nil)
	request = request.WithContext(kata.WithPrincipal(request.Context(), kata.Principal{
		Subject: "user-123",
		Actor:   "Example User",
	}))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.JSONEq(t, `{"status":404,"error":{"code":"not_found","message":"resource not found"}}`,
		response.Body.String())
	require.Len(t, controller.snapshot(), 1)
	assert.Equal(t, kata.Operation{
		ID:         "showProject",
		Method:     http.MethodGet,
		Path:       "/api/v1/projects/{project_id}",
		PathParams: map[string]string{"project_id": "42"},
		Policy: kata.OperationPolicy{
			Kind: kata.OperationProjectRead, Capability: kata.CapabilityRead,
		},
		ProjectIDs: []int64{42},
	}, controller.snapshot()[0].Operation)
}

func TestServiceAccessControllerReceivesOperationPolicy(t *testing.T) {
	controller := &recordingAccessController{}
	service, err := kata.New(context.Background(), kata.Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	project, err := service.EnsureProject(context.Background(), kata.ProjectSpec{
		UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Name: "example-project",
	})
	require.NoError(t, err)

	principal := kata.Principal{Subject: "user-123", Actor: "Example User"}
	serve := func(method, target string, body []byte) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, target, bytes.NewReader(body))
		if len(body) > 0 {
			request.Header.Set("Content-Type", "application/json")
		}
		request = request.WithContext(kata.WithPrincipal(request.Context(), principal))
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		return response
	}

	response := serve(http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.Project.ID, 10)+"/issues", nil)
	require.Equal(t, http.StatusOK, response.Code)
	response = serve(http.MethodPost,
		"/api/v1/projects/"+strconv.FormatInt(project.Project.ID, 10)+"/issues",
		[]byte(`{"actor":"ignored","title":"Classified task mutation"}`))
	require.Equal(t, http.StatusOK, response.Code)
	response = serve(http.MethodGet, "/api/v1/tokens", nil)
	require.Equal(t, http.StatusForbidden, response.Code)

	requests := controller.snapshot()
	require.Len(t, requests, 3)
	assert.Equal(t, kata.OperationPolicy{
		Kind: kata.OperationTaskRead, Capability: kata.CapabilityRead,
	}, requests[0].Operation.Policy)
	assert.Equal(t, kata.OperationPolicy{
		Kind: kata.OperationTaskMutation, Capability: kata.CapabilityWrite, Mutation: true,
	}, requests[1].Operation.Policy)
	assert.Equal(t, kata.OperationPolicy{
		Kind: kata.OperationTokenAdministration, Capability: kata.CapabilityManage,
	}, requests[2].Operation.Policy)
}

func TestRestrictedEmbeddingProfileDeniesNativeAdministration(t *testing.T) {
	controller := &recordingAccessController{}
	service, err := kata.New(context.Background(), kata.Config{
		DSN: filepath.Join(t.TempDir(), "service.db"), Access: controller,
		Profile: kata.EmbeddingProfileRestricted,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	project, err := service.EnsureProject(context.Background(), kata.ProjectSpec{
		UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Name: "example-project",
	})
	require.NoError(t, err)
	principal := kata.Principal{Subject: "user-123", Actor: "Example User"}
	serve := func(method, target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, target, nil)
		request = request.WithContext(kata.WithPrincipal(request.Context(), principal))
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		return response
	}

	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/projects"},
		{http.MethodGet, "/api/v1/tokens"},
		{http.MethodPost, "/api/v1/federation/enrollments"},
		{http.MethodPost, "/api/v1/projects/" + strconv.FormatInt(project.Project.ID, 10) +
			"/issue-sync/github/enable"},
	} {
		response := serve(request.method, request.path)
		assert.Equal(t, http.StatusNotFound, response.Code, "%s %s", request.method, request.path)
		assert.JSONEq(t, `{"status":404,"error":{"code":"not_found","message":"resource not found"}}`,
			response.Body.String())
	}

	response := serve(http.MethodGet,
		"/api/v1/projects/"+strconv.FormatInt(project.Project.ID, 10)+"/issues")
	assert.Equal(t, http.StatusOK, response.Code)
	requests := controller.snapshot()
	require.Len(t, requests, 1, "restricted operations must fail before host authorization")
	assert.Equal(t, "listIssues", requests[0].Operation.ID)
}

func TestNewRejectsUnknownEmbeddingProfile(t *testing.T) {
	_, err := kata.New(context.Background(), kata.Config{
		DSN:     filepath.Join(t.TempDir(), "service.db"),
		Auth:    kata.AuthConfig{TrustCallerAuthentication: true},
		Profile: kata.EmbeddingProfile("unknown"),
	})
	require.EqualError(t, err, `kata: unknown embedding profile "unknown"`)
}

func TestServiceAccessControllerSuppliesTheMutationActor(t *testing.T) {
	controller := &recordingAccessController{}
	service, err := kata.New(context.Background(), kata.Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	principal := kata.Principal{Subject: "user-123", Actor: "Example User"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.Handler().ServeHTTP(w, r.WithContext(kata.WithPrincipal(r.Context(), principal)))
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	project := createProject(t, server.URL, "example-project")
	require.NotZero(t, project.Project.ID)

	body, err := json.Marshal(map[string]string{
		"actor": "caller supplied actor",
		"title": "Host-authorized change",
	})
	require.NoError(t, err)
	response, err := http.Post(
		server.URL+"/api/v1/projects/"+strconv.FormatInt(project.Project.ID, 10)+"/issues",
		"application/json",
		bytes.NewReader(body),
	)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)

	var created struct {
		Issue struct {
			Author string `json:"author"`
		} `json:"issue"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&created))
	assert.Equal(t, "Example User", created.Issue.Author)

	requests := controller.snapshot()
	require.GreaterOrEqual(t, len(requests), 2)
	issueRequest := requests[len(requests)-1]
	assert.Equal(t, principal, issueRequest.Principal)
	assert.Equal(t, "createIssue", issueRequest.Operation.ID)
	assert.Equal(t, strconv.FormatInt(project.Project.ID, 10), issueRequest.Operation.PathParams["project_id"])
}

func TestServiceAccessControllerKeepsUnexpectedFailuresDistinctFromDenials(t *testing.T) {
	controller := &recordingAccessController{err: errors.New("policy store unavailable")}
	service, err := kata.New(context.Background(), kata.Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	request = request.WithContext(kata.WithPrincipal(request.Context(), kata.Principal{
		Subject: "user-123",
		Actor:   "Example User",
	}))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.JSONEq(t, `{"status":503,"error":{"code":"access_unavailable","message":"access decision unavailable"}}`,
		response.Body.String())
}

func TestServiceAccessLeaseStopsAStreamBeforeTheNextEvent(t *testing.T) {
	lease := &revocableAccessLease{}
	controller := &recordingAccessController{lease: lease}
	service, err := kata.New(context.Background(), kata.Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	principal := kata.Principal{Subject: "user-123", Actor: "Example User"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service.Handler().ServeHTTP(w, r.WithContext(kata.WithPrincipal(r.Context(), principal)))
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	project := createProject(t, server.URL, "example-project")
	streamRequest, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/events/stream?after_id=0&project_id="+
			strconv.FormatInt(project.Project.ID, 10), nil)
	require.NoError(t, err)
	streamRequest.Header.Set("Accept", "text/event-stream")
	streamResponse, err := server.Client().Do(streamRequest)
	require.NoError(t, err)
	defer func() { _ = streamResponse.Body.Close() }()
	require.Equal(t, http.StatusOK, streamResponse.StatusCode)

	reader := bufio.NewReader(streamResponse.Body)
	connected, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, ": connected\n", connected)
	_, err = reader.ReadString('\n')
	require.NoError(t, err)

	lease.revoked.Store(true)
	body := bytes.NewBufferString(`{"actor":"untrusted","title":"must not reach revoked stream"}`)
	mutation, err := http.Post(server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/issues", "application/json", body)
	require.NoError(t, err)
	defer func() { _ = mutation.Body.Close() }()
	require.Equal(t, http.StatusOK, mutation.StatusCode)

	streamResult := make(chan error, 1)
	go func() {
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				streamResult <- nil
				return
			}
			if line == "event: issue.created\n" {
				streamResult <- errors.New("revoked stream received issue.created")
				return
			}
		}
	}()
	select {
	case streamErr := <-streamResult:
		require.NoError(t, streamErr)
	case <-time.After(2 * time.Second):
		require.Fail(t, "revoked event stream did not close")
	}
}

func TestServiceAccessControllerRequiresALeaseForEventStreams(t *testing.T) {
	controller := &recordingAccessController{}
	service, err := kata.New(context.Background(), kata.Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream?after_id=0", nil)
	request.Header.Set("Accept", "text/event-stream")
	request = request.WithContext(kata.WithPrincipal(request.Context(), kata.Principal{
		Subject: "user-123",
		Actor:   "Example User",
	}))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.JSONEq(t, `{"status":503,"error":{"code":"access_lease_required","message":"long-lived access lease required"}}`,
		response.Body.String())
}

func TestNewRejectsHostAccessCombinedWithAnotherAuthenticationPolicy(t *testing.T) {
	controller := &recordingAccessController{}
	for _, auth := range []kata.AuthConfig{
		{Token: "service-token"},
		{TrustCallerAuthentication: true},
	} {
		service, err := kata.New(context.Background(), kata.Config{
			DSN: filepath.Join(t.TempDir(), "service.db"), Access: controller, Auth: auth,
		})
		assert.Nil(t, service)
		assert.EqualError(t, err,
			"kata: auth token, trusted caller authentication, and host access are mutually exclusive")
	}
}

func TestWithPrincipalDoesNotChangeTrustedCallerMode(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := kata.WithPrincipal(r.Context(), kata.Principal{
			Subject: "must-be-ignored", Actor: "must-be-ignored",
		})
		service.Handler().ServeHTTP(w, r.WithContext(ctx))
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	project := createProject(t, server.URL, "example-project")

	body := bytes.NewBufferString(`{"actor":"request actor","title":"ordinary trusted mutation"}`)
	response, err := http.Post(server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/issues", "application/json", body)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)

	var created struct {
		Issue struct {
			Author string `json:"author"`
		} `json:"issue"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&created))
	assert.Equal(t, "request actor", created.Issue.Author)
}

func TestServiceAccessControllerProtectsTheOpenAPIDocument(t *testing.T) {
	controller := &recordingAccessController{}
	service, err := kata.New(context.Background(), kata.Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	missing := httptest.NewRecorder()
	service.Handler().ServeHTTP(missing,
		httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	assert.Equal(t, http.StatusUnauthorized, missing.Code)

	request := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	principal := kata.Principal{Subject: "user-123", Actor: "Example User"}
	request = request.WithContext(kata.WithPrincipal(request.Context(), principal))
	allowed := httptest.NewRecorder()
	service.Handler().ServeHTTP(allowed, request)
	require.Equal(t, http.StatusOK, allowed.Code)

	requests := controller.snapshot()
	require.Len(t, requests, 1)
	assert.Equal(t, principal, requests[0].Principal)
	assert.Equal(t, kata.Operation{
		ID: "openAPI", Method: http.MethodGet, Path: "/openapi.yaml",
		PathParams: map[string]string{},
		Policy: kata.OperationPolicy{
			Kind: kata.OperationServiceRead, Capability: kata.CapabilityRead,
		},
	}, requests[0].Operation)
}

func TestServiceAccessControllerAuthorizesBothProjectsBeforeMerge(t *testing.T) {
	controller := &recordingAccessController{}
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

	target := createProject(t, server.URL, "target-project")
	source := createProject(t, server.URL, "source-project")
	controller.authorize = func(request kata.AccessRequest) error {
		if request.Operation.ID == "mergeProject" &&
			containsInt64(request.Operation.ProjectIDs, source.Project.ID) {
			return kata.ErrAccessDenied
		}
		return nil
	}

	body := bytes.NewBufferString(`{"source_project_id":` +
		strconv.FormatInt(source.Project.ID, 10) + `}`)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/projects/"+
		strconv.FormatInt(target.Project.ID, 10)+"/merge", body)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, response.StatusCode)

	requests := controller.snapshot()
	mergeRequest := requests[len(requests)-1]
	assert.Equal(t, "mergeProject", mergeRequest.Operation.ID)
	assert.ElementsMatch(t, []int64{target.Project.ID, source.Project.ID}, mergeRequest.Operation.ProjectIDs)
}

func TestServiceAccessControllerAuthorizesMoveDestination(t *testing.T) {
	controller := &recordingAccessController{}
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

	source := createProject(t, server.URL, "source-project")
	target := createProject(t, server.URL, "target-project")
	issueBody := bytes.NewBufferString(`{"actor":"ignored","title":"stays in source"}`)
	issueResponse, err := http.Post(server.URL+"/api/v1/projects/"+
		strconv.FormatInt(source.Project.ID, 10)+"/issues", "application/json", issueBody)
	require.NoError(t, err)
	defer func() { _ = issueResponse.Body.Close() }()
	require.Equal(t, http.StatusOK, issueResponse.StatusCode)
	var created struct {
		Issue struct {
			ShortID  string `json:"short_id"`
			Revision int64  `json:"revision"`
		} `json:"issue"`
	}
	require.NoError(t, json.NewDecoder(issueResponse.Body).Decode(&created))

	controller.authorize = func(request kata.AccessRequest) error {
		if request.Operation.ID == "moveIssue" &&
			containsString(request.Operation.ProjectUIDs, target.Project.UID) {
			return kata.ErrAccessDenied
		}
		return nil
	}
	moveBody := bytes.NewBufferString(`{"actor":"ignored","to_project_uid":"` + target.Project.UID + `"}`)
	moveRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/projects/"+
		strconv.FormatInt(source.Project.ID, 10)+"/issues/"+created.Issue.ShortID+"/actions/move", moveBody)
	require.NoError(t, err)
	moveRequest.Header.Set("Content-Type", "application/json")
	moveRequest.Header.Set("If-Match", `"rev-`+strconv.FormatInt(created.Issue.Revision, 10)+`"`)
	moveResponse, err := server.Client().Do(moveRequest)
	require.NoError(t, err)
	defer func() { _ = moveResponse.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, moveResponse.StatusCode)

	requests := controller.snapshot()
	request := requests[len(requests)-1]
	assert.Equal(t, "moveIssue", request.Operation.ID)
	assert.Equal(t, []int64{source.Project.ID}, request.Operation.ProjectIDs)
	assert.Equal(t, []string{target.Project.UID}, request.Operation.ProjectUIDs)
}

func TestServiceAccessControllerRequiresCompleteEventStreamScope(t *testing.T) {
	controller := &recordingAccessController{lease: &revocableAccessLease{}}
	controller.authorize = func(request kata.AccessRequest) error {
		if request.Operation.ID == "streamEvents" {
			return kata.ErrAccessDenied
		}
		return nil
	}
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
	project := createProject(t, server.URL, "example-project")

	for _, path := range []string{
		"/api/v1/events/stream?project_id=" + strconv.FormatInt(project.Project.ID, 10),
		"/api/v1/events/stream",
	} {
		request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		require.NoError(t, err)
		request.Header.Set("Accept", "text/event-stream")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		_ = response.Body.Close()
		assert.Equal(t, http.StatusNotFound, response.StatusCode)
	}

	requests := controller.snapshot()
	require.GreaterOrEqual(t, len(requests), 3)
	projectStream := requests[len(requests)-2].Operation
	globalStream := requests[len(requests)-1].Operation
	assert.Empty(t, projectStream.ProjectIDs)
	assert.True(t, projectStream.AllProjects)
	assert.Empty(t, globalStream.ProjectIDs)
	assert.True(t, globalStream.AllProjects)
}

func TestServiceAccessControllerPreservesScopedFederationAuthentication(t *testing.T) {
	controller := &recordingAccessController{}
	service, err := kata.New(context.Background(), kata.Config{
		DSN: filepath.Join(t.TempDir(), "service.db"), Access: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	principal := kata.Principal{Subject: "user-123", Actor: "Example User"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r = r.WithContext(kata.WithPrincipal(r.Context(), principal))
		}
		service.Handler().ServeHTTP(w, r)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	project := createProject(t, server.URL, "federated-project")
	issueBody := bytes.NewBufferString(`{"actor":"ignored","title":"claimable issue"}`)
	issueResponse, err := http.Post(server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/issues", "application/json", issueBody)
	require.NoError(t, err)
	defer func() { _ = issueResponse.Body.Close() }()
	require.Equal(t, http.StatusOK, issueResponse.StatusCode)
	var issue struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.NewDecoder(issueResponse.Body).Decode(&issue))

	postJSON(t, server.Client(), server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/federation/enable",
		`{"actor":"ignored"}`, nil)
	var enrollment struct {
		Token string `json:"token"`
	}
	postJSON(t, server.Client(), server.URL+"/api/v1/federation/enrollments",
		`{"spoke_instance_uid":"01HZNQ7VFPK1XGD8R5MABCD4EA","project_id":`+
			strconv.FormatInt(project.Project.ID, 10)+
			`,"capabilities":"pull,claim","actor":"ignored"}`, &enrollment)
	require.NotEmpty(t, enrollment.Token)

	invalidMetadataRequest, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/federation/metadata", nil)
	require.NoError(t, err)
	invalidMetadataRequest.Header.Set("Authorization", "Bearer invalid-token")
	invalidMetadataResponse, err := server.Client().Do(invalidMetadataRequest)
	require.NoError(t, err)
	defer func() { _ = invalidMetadataResponse.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, invalidMetadataResponse.StatusCode)

	metadataRequest, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/federation/metadata", nil)
	require.NoError(t, err)
	metadataRequest.Header.Set("Authorization", "Bearer "+enrollment.Token)
	metadataResponse, err := server.Client().Do(metadataRequest)
	require.NoError(t, err)
	defer func() { _ = metadataResponse.Body.Close() }()
	assert.Equal(t, http.StatusOK, metadataResponse.StatusCode)

	invalidClaimRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/issues/"+issue.Issue.ShortID+
		"/lease/actions/acquire", bytes.NewBufferString(
		`{"holder":"unauthorized-worker","client_kind":"cli","claim_kind":"hard"}`))
	require.NoError(t, err)
	invalidClaimRequest.Header.Set("Content-Type", "application/json")
	invalidClaimRequest.Header.Set("Authorization", "Bearer invalid-token")
	invalidClaimResponse, err := server.Client().Do(invalidClaimRequest)
	require.NoError(t, err)
	defer func() { _ = invalidClaimResponse.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, invalidClaimResponse.StatusCode)

	claimRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/issues/"+issue.Issue.ShortID+
		"/lease/actions/acquire", bytes.NewBufferString(
		`{"holder":"spoke-worker","client_kind":"cli","claim_kind":"hard"}`))
	require.NoError(t, err)
	claimRequest.Header.Set("Content-Type", "application/json")
	claimRequest.Header.Set("Authorization", "Bearer "+enrollment.Token)
	claimResponse, err := server.Client().Do(claimRequest)
	require.NoError(t, err)
	defer func() { _ = claimResponse.Body.Close() }()
	assert.Equal(t, http.StatusOK, claimResponse.StatusCode)

	statusRequest, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/issues/"+issue.Issue.ShortID+"/lease", nil)
	require.NoError(t, err)
	statusRequest.Header.Set("Authorization", "Bearer "+enrollment.Token)
	statusResponse, err := server.Client().Do(statusRequest)
	require.NoError(t, err)
	defer func() { _ = statusResponse.Body.Close() }()
	assert.Equal(t, http.StatusOK, statusResponse.StatusCode)

	for _, request := range controller.snapshot() {
		assert.NotEqual(t, "getFederationProjectMetadata", request.Operation.ID)
		assert.NotEqual(t, "acquireIssueLease", request.Operation.ID)
		assert.NotEqual(t, "getIssueLeaseStatus", request.Operation.ID)
	}
}

func TestServiceAccessControllerSuppliesForceReleaseActor(t *testing.T) {
	controller := &recordingAccessController{}
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

	project := createProject(t, server.URL, "claim-project")
	issueBody := bytes.NewBufferString(`{"actor":"ignored","title":"claimed issue"}`)
	issueResponse, err := http.Post(server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/issues", "application/json", issueBody)
	require.NoError(t, err)
	defer func() { _ = issueResponse.Body.Close() }()
	require.Equal(t, http.StatusOK, issueResponse.StatusCode)
	var issue struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.NewDecoder(issueResponse.Body).Decode(&issue))
	postJSON(t, server.Client(), server.URL+"/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/federation/enable",
		`{"actor":"forged enable actor"}`, nil)

	claimPath := server.URL + "/api/v1/projects/" + strconv.FormatInt(project.Project.ID, 10) +
		"/issues/" + issue.Issue.ShortID + "/lease/actions/"
	postJSON(t, server.Client(), claimPath+"acquire",
		`{"holder":"local-worker","client_kind":"cli","claim_kind":"hard"}`, nil)
	var released struct {
		Event struct {
			Actor string `json:"actor"`
		} `json:"event"`
	}
	postJSON(t, server.Client(), claimPath+"force_release",
		`{"actor":"forged audit actor","reason":"operator override"}`, &released)
	assert.Equal(t, "Example User", released.Event.Actor)
}

func postJSON(t *testing.T, client *http.Client, url, body string, out any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	if out != nil {
		require.NoError(t, json.NewDecoder(response.Body).Decode(out))
	}
}

func containsInt64(values []int64, want int64) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
