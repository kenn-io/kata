package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
)

func TestSearchEndpoint_ReturnsHitsWithScores(t *testing.T) {
	env := testenv.New(t)
	pid := initLocalWorkspace(t, env, "kata")
	createIssueViaHTTP(t, env, pid, "fix login crash on Safari")
	createIssueViaHTTP(t, env, pid, "unrelated")

	resp, bs := envGetRaw(t, env, projectPath(pid)+"/search?q="+url.QueryEscape("login Safari"))
	require.Equal(t, 200, resp.StatusCode)
	body := string(bs)
	assert.Contains(t, body, `"query":"login Safari"`)
	assert.Contains(t, body, `"title":"fix login crash on Safari"`)
	assert.Contains(t, body, `"matched_in"`)
	assert.NotContains(t, body, `"title":"unrelated"`,
		"unrelated issue should not appear in results")
}

func TestSearchEndpoint_InsecureReadonlyUnauthenticatedAutoSearchStaysLexical(t *testing.T) {
	var embeddingCalls atomic.Int32
	embedderSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		embeddingCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{1, 0}}}})
	}))
	defer embedderSrv.Close()
	emb, err := embedding.New(embedding.Config{BaseURL: embedderSrv.URL, Model: "m", Dims: 2})
	require.NoError(t, err)

	env := testenv.New(t, testenv.WithInsecureReadonly(), func(cfg *daemon.ServerConfig) {
		cfg.Embedder = emb
	})
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "index credential rotation",
		Body:      "keep provider calls behind daemon credentials",
		Author:    "agent",
	})
	require.NoError(t, err)

	resp, bs := envGetRaw(t, env, projectPath(project.ID)+"/search?q="+url.QueryEscape("credential rotation"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"mode":"lexical"`)
	assert.Equal(t, int32(0), embeddingCalls.Load(), "unauthenticated read-only search must not call embedding provider")
}

func TestSearchEndpoint_InsecureReadonlyUnauthenticatedExplicitVectorModesRequireAuth(t *testing.T) {
	var embeddingCalls atomic.Int32
	embedderSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		embeddingCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{1, 0}}}})
	}))
	defer embedderSrv.Close()
	emb, err := embedding.New(embedding.Config{BaseURL: embedderSrv.URL, Model: "m", Dims: 2})
	require.NoError(t, err)

	env := testenv.New(t, testenv.WithInsecureReadonly(), func(cfg *daemon.ServerConfig) {
		cfg.Embedder = emb
	})
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)

	for _, mode := range []string{"semantic", "hybrid"} {
		resp, bs := envGetRaw(t, env, projectPath(project.ID)+"/search?q=anything&mode="+mode)
		assertAPIError(t, resp.StatusCode, bs, http.StatusUnauthorized, "auth_required")
	}
	assert.Equal(t, int32(0), embeddingCalls.Load(), "rejected semantic search must not call embedding provider")
}

func TestSearchEndpoint_EmptyQueryIsValidationError(t *testing.T) {
	env := testenv.New(t)
	pid := initLocalWorkspace(t, env, "kata")

	resp, bs := envGetRaw(t, env, projectPath(pid)+"/search?q=")
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

func TestSearchEndpoint_UnknownProjectIs404(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envGetRaw(t, env, "/api/v1/projects/9999/search?q=anything")
	assertAPIError(t, resp.StatusCode, bs, 404, "project_not_found")
}

// TestSearchEndpoint_EmptyResultsIsArrayNotNull pins the wire shape: a
// search with no matches must return "results":[] (a JSON array, possibly
// empty), not "results":null. CLI consumers iterate over the slice and a
// future regression that flipped to `var hits []SearchHit` would silently
// emit null and break clients that assume an array.
func TestSearchEndpoint_EmptyResultsIsArrayNotNull(t *testing.T) {
	env := testenv.New(t)
	pid := initLocalWorkspace(t, env, "kata")

	resp, bs := envGetRaw(t, env, projectPath(pid)+"/search?q=zxqyq-no-such-token")
	require.Equal(t, 200, resp.StatusCode)
	body := string(bs)
	assert.Contains(t, body, `"results":[]`,
		"empty results must serialize as an array, not null")
	assert.NotContains(t, body, `"results":null`)
}
