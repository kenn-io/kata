package daemon_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

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
	postLabel(t, env, pid, a, "priority:high")
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
	assert.Equal(t, int64(1), got["priority:high"])
}

// issueLabelNames reads the labels straight from the store so tests check
// what is persisted, not what the response claims.
func issueLabelNames(t *testing.T, env *testenv.Env, issueID int64) []string {
	t.Helper()
	rows, err := env.DB.LabelsByIssue(t.Context(), issueID)
	require.NoError(t, err)
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Label)
	}
	return names
}

func TestUpdateLabels_SwapsTriageLabels(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	postLabel(t, env, pid, n, "ready-for-agent")
	postLabel(t, env, pid, n, "keep")

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePath(pid, n, "labels"),
		map[string]any{"actor": "tester", "add": []string{"ready-for-human"}, "remove": []string{"ready-for-agent"}}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out struct {
		Labels []string `json:"labels"`
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, []string{"keep", "ready-for-human"}, out.Labels)
	assert.True(t, out.Changed)
	require.Len(t, out.Events, 2)
	assert.Equal(t, "issue.unlabeled", out.Events[0].Type)
	assert.Equal(t, "issue.labeled", out.Events[1].Type)
	assert.ElementsMatch(t, []string{"keep", "ready-for-human"}, issueLabelNames(t, env, n))
}

func TestUpdateLabels_NoOpReportsLabelsWithoutEvents(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	postLabel(t, env, pid, n, "bug")

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePath(pid, n, "labels"),
		map[string]any{"actor": "tester", "add": []string{"bug", "bug"}, "remove": []string{"absent"}}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out struct {
		Labels  []string `json:"labels"`
		Events  []any    `json:"events"`
		Changed bool     `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, []string{"bug"}, out.Labels)
	assert.Empty(t, out.Events)
	assert.False(t, out.Changed)
}

func TestUpdateLabels_SameLabelInAddAndRemoveIs400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	postLabel(t, env, pid, n, "ready-for-agent")

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePath(pid, n, "labels"),
		map[string]any{"actor": "tester", "add": []string{"ready-for-agent"}, "remove": []string{"ready-for-agent"}}, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
	assert.Contains(t, string(raw), "ready-for-agent")
	assert.Equal(t, []string{"ready-for-agent"}, issueLabelNames(t, env, n))
}

func TestUpdateLabels_InvalidAddKeepsRemovedLabelAttached(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	postLabel(t, env, pid, n, "ready-for-agent")

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePath(pid, n, "labels"),
		map[string]any{"actor": "tester", "add": []string{"Bad-Case"}, "remove": []string{"ready-for-agent"}}, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
	assert.Equal(t, []string{"ready-for-agent"}, issueLabelNames(t, env, n))
}

func TestUpdateLabels_EmptyOrBlankListsAre400(t *testing.T) {
	env := testenv.New(t)
	pid, n := setupOneIssue(t, env)
	postLabel(t, env, pid, n, "bug")

	for name, body := range map[string]map[string]any{
		"both empty": {"actor": "tester", "add": []string{}, "remove": []string{}},
		"no lists":   {"actor": "tester"},
		"blank add":  {"actor": "tester", "add": []string{"  "}},
		"blank rm":   {"actor": "tester", "add": []string{"x"}, "remove": []string{""}},
	} {
		t.Run(name, func(t *testing.T) {
			resp, raw := envDoRaw(t, env, http.MethodPatch, issuePath(pid, n, "labels"), body, nil)
			assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
		})
	}
	assert.Equal(t, []string{"bug"}, issueLabelNames(t, env, n))
}
