package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestTokenIdentity_OverridesClientActor(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(context.Background(), "identity-e2e")
	require.NoError(t, err)

	createTokenResp := doJSONWithBearer(t, env.HTTP, http.MethodPost, env.URL+"/api/v1/tokens",
		"bootstrap-token", map[string]any{"actor": "alice", "name": "laptop"})
	require.Equal(t, http.StatusOK, createTokenResp.status, createTokenResp.body)
	var tokenOut struct {
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal([]byte(createTokenResp.body), &tokenOut))
	require.NotEmpty(t, tokenOut.Plaintext)

	createIssueResp := doJSONWithBearer(t, env.HTTP, http.MethodPost,
		env.URL+"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues",
		tokenOut.Plaintext, map[string]any{"actor": "mallory", "title": "identity override"})
	require.Equal(t, http.StatusOK, createIssueResp.status, createIssueResp.body)
	var mutation struct {
		Event struct {
			Actor string `json:"actor"`
		} `json:"event"`
		Issue struct {
			Author string `json:"author"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(createIssueResp.body), &mutation))
	assert.Equal(t, "alice", mutation.Event.Actor)
	assert.Equal(t, "alice", mutation.Issue.Author)
}

type rawHTTPResponse struct {
	status int
	body   string
}

func doJSONWithBearer(t *testing.T, client *http.Client, method, url, bearer string, body any) rawHTTPResponse {
	t.Helper()
	bs, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), method, url, bytes.NewReader(bs))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req) //nolint:gosec // test-only loopback URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return rawHTTPResponse{status: resp.StatusCode, body: drain(t, resp)}
}
