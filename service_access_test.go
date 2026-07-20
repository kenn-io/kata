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
	mu       sync.Mutex
	requests []kata.AccessRequest
	err      error
	lease    kata.AccessLease
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
	}, controller.snapshot()[0].Operation)
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
	}, requests[0].Operation)
}
