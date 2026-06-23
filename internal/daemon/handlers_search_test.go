package daemon_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
