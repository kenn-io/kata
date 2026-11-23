package daemon_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestAddLabel_PriorityLabelRejected(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	issue, err := env.DB.IssueByID(t.Context(), n)
	require.NoError(t, err)
	for _, label := range []string{"p0", "p1", "p2", "p3", "p4", "priority:high", "priority:"} {
		t.Run(label, func(t *testing.T) {
			resp, body := envDoRaw(t, env, http.MethodPost, issuePath(pid, n, "labels"), map[string]string{"actor": "tester", "label": label}, nil)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, string(body), "kata edit kata#"+issue.ShortID+" --priority N")
		})
	}
	for _, label := range []string{"p5", "priority", "area:ui"} {
		postLabel(t, env, pid, n, label)
	}
	resp, body := envDoRaw(t, env, http.MethodPost, issuePathRef(pid, issue.UID, "labels"), map[string]string{"actor": "tester", "label": "p2"}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "kata edit kata#"+issue.ShortID+" --priority N")
}

func TestAddLabel_ExistingPriorityLabelIsNoop(t *testing.T) {
	env := testenv.New(t)
	pid, id := setupOneIssue(t, env)
	_, _, err := env.DB.AddLabelAndEvent(t.Context(), id, db.LabelEventParams{EventType: "issue.labeled", Label: "p2", Actor: "tester"})
	require.NoError(t, err)
	resp, body := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "labels"), map[string]string{"actor": "tester", "label": "p2"}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"changed":false`)
}

func TestAddLabel_PriorityLabelMissingIssue(t *testing.T) {
	env := testenv.New(t)
	pid, _ := setupOneIssue(t, env)
	resp := envDoJSON(t, env, http.MethodPost, issuePathRef(pid, "zzzz", "labels"), map[string]string{"actor": "tester", "label": "p2"}, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAddLabel_HappyPath(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	out := postLabel(t, env, pid, n, "needs-review")
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.labeled", out.Event.Type)
	assert.True(t, out.Changed)
	assert.Equal(t, "needs-review", out.Label.Label)
}

func TestAddLabel_DuplicateIsNoOp(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	postLabel(t, env, pid, n, "bug")
	out := postLabel(t, env, pid, n, "bug")
	assert.Nil(t, out.Event)
	assert.False(t, out.Changed)
	// No-op response must still carry the existing label row, not a zero value.
	assert.Equal(t, "bug", out.Label.Label)
}

func TestAddLabel_InvalidIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp := envDoJSON(t, env, http.MethodPost, issuePath(pid, n, "labels"),
		map[string]string{"actor": "tester", "label": "Bad-Case"}, nil)
	assert.Equal(t, 400, resp.StatusCode)
}

func TestRemoveLabel_HappyPath(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	postLabel(t, env, pid, n, "bug")

	resp, out := deleteLabel(t, env, pid, n, "bug")
	require.Equal(t, 200, resp.StatusCode)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.unlabeled", out.Event.Type)
	assert.True(t, out.Changed)
}

func TestRemoveLabel_AbsentIs200NoOp(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, out := deleteLabel(t, env, pid, n, "never-attached")
	require.Equal(t, 200, resp.StatusCode)
	assert.Nil(t, out.Event)
	assert.False(t, out.Changed)
}

func TestAddLabel_BlankActorIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp := envDoJSON(t, env, http.MethodPost, issuePath(pid, n, "labels"),
		map[string]string{"actor": "   ", "label": "bug"}, nil)
	assert.Equal(t, 400, resp.StatusCode)
}

func TestRemoveLabel_BlankActorIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	resp, _ := deleteLabelAs(t, env, pid, n, "  ", "bug")
	assert.Equal(t, 400, resp.StatusCode)
}

func TestLabelsList_ReturnsCounts(t *testing.T) {
	env := testenv.New(t)
	pid, a := setupOneIssue(t, env)
	b := createIssueViaHTTP(t, env, pid, "b")
	postLabel(t, env, pid, a, "bug")
	postLabel(t, env, pid, a, "area:ui")
	postLabel(t, env, pid, b, "bug")

	var out struct {
		Labels []struct {
			Label string `json:"label"`
			Count int64  `json:"count"`
		} `json:"labels"`
	}
	envGetJSON(t, env, projectPath(pid)+"/labels", &out)
	got := map[string]int64{}
	for _, c := range out.Labels {
		got[c.Label] = c.Count
	}
	assert.Equal(t, int64(2), got["bug"])
	assert.Equal(t, int64(1), got["area:ui"])
}
