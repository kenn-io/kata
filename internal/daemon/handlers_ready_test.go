package daemon_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/testenv"
)

// readyResp is the decoded shape of a /ready response body, narrowed to the
// fields the tests assert on.
type readyResp struct {
	Issues []struct {
		ShortID string `json:"short_id"`
	} `json:"issues"`
}

// getReady GETs /api/v1/projects/{pid}/ready{query} and decodes the response.
// query may be empty or a leading-`?` query string (e.g. "?limit=2").
func getReady(t *testing.T, env *testenv.Env, projectID int64, query string) readyResp {
	t.Helper()
	var out readyResp
	envGetJSON(t, env, projectPath(projectID)+"/ready"+query, &out)
	return out
}

func TestReady_FiltersBlocked(t *testing.T) {
	env := testenv.New(t)
	pid, blocker, blocked := setupTwoIssues(t, env)
	standalone := createIssueViaHTTP(t, env, pid, "standalone")
	postLink(t, env, pid, blocker, "blocks", blocked)
	blockerShort := refForIssue(t, env, blocker)
	blockedShort := refForIssue(t, env, blocked)
	standaloneShort := refForIssue(t, env, standalone)

	out := getReady(t, env, pid, "")
	got := map[string]bool{}
	for _, i := range out.Issues {
		got[i.ShortID] = true
	}
	assert.True(t, got[blockerShort], "blocker is ready")
	assert.True(t, got[standaloneShort], "standalone is ready")
	assert.False(t, got[blockedShort], "blocked while blocker is open")
}

func TestReady_RespectsLimit(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	for i := 0; i < 3; i++ {
		createIssueViaHTTP(t, env, pid, "x")
	}

	out := getReady(t, env, pid, "?limit=2")
	assert.Len(t, out.Issues, 2)
}

func TestReady_UnownedAndOwnerMutuallyExclusive(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")

	status, _ := env.Get(t, projectPath(pid)+"/ready?unowned=true&owner=alice")
	assert.Equal(t, 400, status)
}
