package daemon_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// readyResp is the decoded shape of a /ready response body, narrowed to the
// fields the tests assert on.
type readyResp struct {
	Issues []struct {
		ShortID string   `json:"short_id"`
		Labels  []string `json:"labels"`
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
	pid := initLocalWorkspace(t, env, "kata")
	for i := 0; i < 3; i++ {
		createIssueViaHTTP(t, env, pid, "x")
	}

	out := getReady(t, env, pid, "?limit=2")
	assert.Len(t, out.Issues, 2)
}

func TestReady_UnownedAndOwnerMutuallyExclusive(t *testing.T) {
	env := testenv.New(t)
	pid := initLocalWorkspace(t, env, "kata")

	status, _ := env.Get(t, projectPath(pid)+"/ready?unowned=true&owner=alice")
	assert.Equal(t, 400, status)
}

// TestReady_HydratesLabels pins that ready rows carry attached labels the
// way list rows do, so `kata ready` human output can render label chips.
func TestReady_HydratesLabels(t *testing.T) {
	env := testenv.New(t)
	pid := initLocalWorkspace(t, env, "kata")
	labeled := createIssueViaHTTP(t, env, pid, "labeled")
	createIssueViaHTTP(t, env, pid, "bare")
	postLabel(t, env, pid, labeled, "epic")
	postLabel(t, env, pid, labeled, "bug")
	labeledShort := refForIssue(t, env, labeled)

	out := getReady(t, env, pid, "")
	byShort := map[string][]string{}
	for _, i := range out.Issues {
		byShort[i.ShortID] = i.Labels
	}
	assert.ElementsMatch(t, []string{"bug", "epic"}, byShort[labeledShort],
		"ready rows carry the issue's labels")
}

// readyGlobalResp narrows the global response body to the fields these tests
// assert on.
type readyGlobalResp struct {
	Issues []struct {
		ShortID     string   `json:"short_id"`
		ProjectName string   `json:"project_name"`
		Labels      []string `json:"labels"`
	} `json:"issues"`
}

func getReadyGlobal(t *testing.T, env *testenv.Env, query string) readyGlobalResp {
	t.Helper()
	var out readyGlobalResp
	envGetJSON(t, env, "/api/v1/ready"+query, &out)
	return out
}

func TestReadyGlobal_ReturnsIssuesFromAllProjects(t *testing.T) {
	env := testenv.New(t)
	pid1, _, _ := setupTwoIssues(t, env)
	pid2 := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/other.git")
	createIssueViaHTTP(t, env, pid2, "from second project")

	out := getReadyGlobal(t, env, "")

	projects := map[string]bool{}
	for _, i := range out.Issues {
		projects[i.ProjectName] = true
	}
	assert.GreaterOrEqual(t, len(projects), 2,
		"global ready returns issues from at least two projects, got %v", projects)
	_ = pid1
}

func TestReadyGlobal_ExcludesArchivedProjects(t *testing.T) {
	env := testenv.New(t)
	pid1, _, _ := setupTwoIssues(t, env)
	pid2 := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/other.git")
	createIssueViaHTTP(t, env, pid2, "doomed")

	// Look up pid2's name BEFORE archiving so we can assert no row carries it.
	p2, err := env.DB.ProjectByID(t.Context(), pid2)
	require.NoError(t, err)

	// Archive pid2 directly via the DB (bypasses the open-issues guard with
	// Force=true). This is the same pattern as the unit tests in
	// internal/db/queries_ready_test.go.
	_, _, err = env.DB.RemoveProject(t.Context(), db.RemoveProjectParams{
		ProjectID: pid2,
		Actor:     "tester",
		Force:     true,
	})
	require.NoError(t, err)

	out := getReadyGlobal(t, env, "")
	for _, i := range out.Issues {
		assert.NotEqual(t, p2.Name, i.ProjectName,
			"archived project's issues must not appear in /api/v1/ready")
	}
	_ = pid1
}

// TestReadyGlobal_HydratesLabelsAndKeepsProjectName pins that the global
// ready payload gains labels while still carrying project_name on each row
// (needed for qualified refs).
func TestReadyGlobal_HydratesLabelsAndKeepsProjectName(t *testing.T) {
	env := testenv.New(t)
	pid1 := initLocalWorkspace(t, env, "kata")
	pid2 := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/other.git")
	iss1 := createIssueViaHTTP(t, env, pid1, "p1 labeled")
	iss2 := createIssueViaHTTP(t, env, pid2, "p2 labeled")
	postLabel(t, env, pid1, iss1, "epic")
	postLabel(t, env, pid2, iss2, "bug")
	short1 := refForIssue(t, env, iss1)
	short2 := refForIssue(t, env, iss2)

	out := getReadyGlobal(t, env, "")
	type row struct {
		project string
		labels  []string
	}
	byShort := map[string]row{}
	for _, i := range out.Issues {
		byShort[i.ShortID] = row{project: i.ProjectName, labels: i.Labels}
	}
	require.Contains(t, byShort, short1)
	require.Contains(t, byShort, short2)
	assert.NotEmpty(t, byShort[short1].project, "project_name kept on global rows")
	assert.NotEmpty(t, byShort[short2].project, "project_name kept on global rows")
	assert.Equal(t, []string{"epic"}, byShort[short1].labels,
		"labels scoped to the right project's issue")
	assert.Equal(t, []string{"bug"}, byShort[short2].labels,
		"labels scoped to the right project's issue")
}

func TestReadyGlobal_LimitCapsTotalRows(t *testing.T) {
	env := testenv.New(t)
	pid1 := initLocalWorkspace(t, env, "kata")
	pid2 := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/other.git")
	for i := 0; i < 3; i++ {
		createIssueViaHTTP(t, env, pid1, "p1")
		createIssueViaHTTP(t, env, pid2, "p2")
	}

	out := getReadyGlobal(t, env, "?limit=2")
	assert.Len(t, out.Issues, 2, "limit caps total rows across projects, not per-project")
}
