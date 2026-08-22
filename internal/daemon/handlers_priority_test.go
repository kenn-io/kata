package daemon_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

type priorityResp struct {
	Issue struct {
		Priority *int64 `json:"priority"`
	} `json:"issue"`
	Event *struct {
		Type    string `json:"type"`
		Payload string `json:"payload"`
	} `json:"event"`
	Changed bool `json:"changed"`
}

func postPriority(t *testing.T, env *testenv.Env, projectID, issueNumber int64, actor string, priority *int64) (*http.Response, priorityResp) {
	t.Helper()
	body := map[string]any{"actor": actor}
	if priority != nil {
		body["priority"] = *priority
	}
	var out priorityResp
	resp := envDoJSON(t, env, http.MethodPost, issuePath(projectID, issueNumber, "actions/priority"),
		body, &out)
	return resp, out
}

func TestPriority_SetHappyPath(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	prio := int64(1)
	resp, out := postPriority(t, env, pid, n, "tester", &prio)
	require.Equal(t, 200, resp.StatusCode)
	require.NotNil(t, out.Issue.Priority)
	assert.Equal(t, int64(1), *out.Issue.Priority)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.priority_set", out.Event.Type)
	assert.True(t, out.Changed)
}

func TestPriority_ClearHappyPath(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	prio := int64(2)
	resp, _ := postPriority(t, env, pid, n, "tester", &prio)
	require.Equal(t, 200, resp.StatusCode)

	resp, out := postPriority(t, env, pid, n, "tester", nil)
	require.Equal(t, 200, resp.StatusCode)
	assert.Nil(t, out.Issue.Priority)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.priority_cleared", out.Event.Type)
	assert.True(t, out.Changed)
}

func TestPriority_SamePriorityIsNoOp(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	prio := int64(1)
	resp, _ := postPriority(t, env, pid, n, "tester", &prio)
	require.Equal(t, 200, resp.StatusCode)

	resp, out := postPriority(t, env, pid, n, "tester", &prio)
	require.Equal(t, 200, resp.StatusCode)
	assert.Nil(t, out.Event, "no event when priority unchanged")
	assert.False(t, out.Changed)
}

func TestPriority_OutOfRangeIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	for _, bad := range []int64{-1, 5, 99} {
		resp, _ := postPriority(t, env, pid, n, "tester", &bad)
		assert.Equal(t, 400, resp.StatusCode, "priority %d", bad)
	}
}

func TestPriority_BlankActorIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	prio := int64(0)
	resp, _ := postPriority(t, env, pid, n, "   ", &prio)
	assert.Equal(t, 400, resp.StatusCode)
}
