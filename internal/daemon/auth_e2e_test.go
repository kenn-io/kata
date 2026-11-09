package daemon_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestDaemonE2E_TokenRequired_MissingHeader_401(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("e2e-tok"))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, env.URL+"/api/v1/projects", nil)
	require.NoError(t, err)
	resp, err := env.HTTP.Do(req) //nolint:gosec // test request to loopback URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestDaemonE2E_TokenRequired_CorrectHeader_OK(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("e2e-tok"))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, env.URL+"/api/v1/projects", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer e2e-tok")
	resp, err := env.HTTP.Do(req) //nolint:gosec // test request to loopback URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
