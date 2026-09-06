package kata_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
)

func TestServiceMountRunAndClose(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "service.db")
	service, err := kata.New(context.Background(), kata.Config{
		DSN:  dsn,
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)

	server := httptest.NewServer(service.Handler())
	project := createProject(t, server.URL, "example-project")
	assert.Equal(t, "example-project", project.Project.Name)
	assert.True(t, project.Created)

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()
	select {
	case err := <-runDone:
		require.Failf(t, "Run returned before cancellation", "error: %v", err)
	default:
	}
	cancelRun()
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.Fail(t, "Run did not stop after cancellation")
	}

	server.Close()
	require.NoError(t, service.Close())
	require.NoError(t, service.Close())

	reopened, err := kata.New(context.Background(), kata.Config{
		DSN:  dsn,
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })

	reopenedServer := httptest.NewServer(reopened.Handler())
	t.Cleanup(reopenedServer.Close)
	projects := listProjects(t, reopenedServer.URL)
	require.Len(t, projects.Projects, 1)
	assert.Equal(t, "example-project", projects.Projects[0].Name)
}

func TestServiceHandlerAtMountsAPIAndBrowserApplication(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	handler, err := service.HandlerAt("/tools/tasks")
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	request, err := http.NewRequest(
		http.MethodGet, server.URL+"/tools/tasks?view=today&label=ready", nil,
	)
	require.NoError(t, err)
	request.Header.Set("Accept", "text/html")
	redirect, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = redirect.Body.Close() }()
	assert.Equal(t,
		server.URL+"/tools/tasks/?view=today&label=ready",
		redirect.Request.URL.String(),
	)
	assert.Equal(t, http.StatusOK, redirect.StatusCode)
	body, err := io.ReadAll(redirect.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "Kata UI assets are not built")

	health, err := http.Get(server.URL + "/tools/tasks/api/v1/health")
	require.NoError(t, err)
	defer func() { _ = health.Body.Close() }()
	assert.Equal(t, http.StatusOK, health.StatusCode)

	outside, err := http.Get(server.URL + "/api/v1/health")
	require.NoError(t, err)
	defer func() { _ = outside.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, outside.StatusCode)
}

func TestServiceHandlerAtRejectsInvalidMountPaths(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	for _, mountPath := range []string{
		"", "/", "relative", "/trailing/", "/not/../clean", "/api", "/api/v1",
	} {
		t.Run(strings.ReplaceAll(mountPath, "/", "_"), func(t *testing.T) {
			handler, err := service.HandlerAt(mountPath)
			assert.Nil(t, handler)
			assert.ErrorContains(t, err, "mount path")
		})
	}
}

func TestNewRejectsMissingDSN(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{})

	assert.Nil(t, service)
	assert.EqualError(t, err, "kata: storage DSN is required")
}

func TestNewRejectsMissingAuthenticationPolicy(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN: filepath.Join(t.TempDir(), "service.db"),
	})

	assert.Nil(t, service)
	assert.EqualError(t, err, "kata: auth token is required unless caller authentication is explicitly trusted")
}

func TestNewRejectsInvalidDefaultTimezone(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:             filepath.Join(t.TempDir(), "service.db"),
		DefaultTimezone: "Not/AZone",
		Auth:            kata.AuthConfig{TrustCallerAuthentication: true},
	})

	assert.Nil(t, service)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `default timezone "Not/AZone" is not a valid IANA timezone`)
}

func TestNewRejectsAmbiguousAuthenticationPolicy(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN: filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{
			Token:                     "service-token",
			TrustCallerAuthentication: true,
		},
	})

	assert.Nil(t, service)
	assert.EqualError(t, err,
		"kata: auth token, trusted caller authentication, and host access are mutually exclusive")
}

func TestServiceBearerAuthentication(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{Token: "service-token"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/projects", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)

	authorized, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/projects", nil)
	require.NoError(t, err)
	authorized.Header.Set("Authorization", "Bearer service-token")
	authorizedResponse, err := http.DefaultClient.Do(authorized)
	require.NoError(t, err)
	defer func() { _ = authorizedResponse.Body.Close() }()
	assert.Equal(t, http.StatusOK, authorizedResponse.StatusCode)
}

type projectResponse struct {
	Project struct {
		ID   int64  `json:"id"`
		UID  string `json:"uid"`
		Name string `json:"name"`
	} `json:"project"`
	Created bool `json:"created"`
}

func createProject(t *testing.T, baseURL, name string) projectResponse {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name, "actor": "user-a"})
	require.NoError(t, err)
	resp, err := http.Post(baseURL+"/api/v1/projects", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out projectResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

type projectsResponse struct {
	Projects []struct {
		Name string `json:"name"`
	} `json:"projects"`
}

func listProjects(t *testing.T, baseURL string) projectsResponse {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/v1/projects")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out projectsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}
