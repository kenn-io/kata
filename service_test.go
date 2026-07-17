package kata_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestNewRejectsAmbiguousAuthenticationPolicy(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN: filepath.Join(t.TempDir(), "service.db"),
		Auth: kata.AuthConfig{
			Token:                     "service-token",
			TrustCallerAuthentication: true,
		},
	})

	assert.Nil(t, service)
	assert.EqualError(t, err, "kata: auth token and trusted caller authentication are mutually exclusive")
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
		Name string `json:"name"`
	} `json:"project"`
	Created bool `json:"created"`
}

func createProject(t *testing.T, baseURL, name string) projectResponse {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name})
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
