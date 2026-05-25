package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// TestProjects_ListJSONHasNoNextIssueNumber pins the spec §9.5 invariant on
// the CLI projection: --json output for `kata projects list` must not include
// the removed next_issue_number field.
func TestProjects_ListJSONHasNoNextIssueNumber(t *testing.T) {
	env := testenv.New(t)
	_ = initBoundWorkspace(t, env.URL, "https://github.com/wesm/kata.git")

	out, err := runCmdOutput(t, env, "--json", "projects", "list")
	require.NoError(t, err)

	var got struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotEmpty(t, got.Projects)
	for _, p := range got.Projects {
		_, has := p["next_issue_number"]
		assert.Falsef(t, has, "project %v should not have next_issue_number", p)
	}
}

func TestProjects_RestoreArchivedProjectByName(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "retry-import")
	require.NoError(t, err)
	_, err = env.DB.AttachAlias(ctx, p.ID, "github.com/wesm/retry-import", "git", "/tmp/retry-import")
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "projects", "restore", "retry-import")
	assert.Contains(t, out, "restored project #"+itoa(p.ID)+" (retry-import)")
	assert.Contains(t, out, "run 'kata init' in the workspace to reattach an alias")

	got, err := env.DB.ProjectByName(ctx, "retry-import")
	require.NoError(t, err)
	assert.Equal(t, p.ID, got.ID)
	assert.Nil(t, got.DeletedAt)
}

func TestProjects_RestoreActiveProjectIsNoop(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "active")
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "projects", "restore", "active")
	assert.Contains(t, out, "project #"+itoa(p.ID)+" (active) is already active")
}

// TestProjects_ResetCounterCommandIsAbsent guards against the reset-counter
// command being reintroduced after the v8 cutover removed its underlying
// next_issue_number column (spec §9.5). The daemon's 404 on the endpoint is
// covered separately by Task 11's handler tests.
func TestProjects_ResetCounterCommandIsAbsent(t *testing.T) {
	projects := rootSubcommands()["projects"]
	require.NotNil(t, projects, "projects subcommand must exist")
	for _, sub := range projects.Commands() {
		assert.NotEqualf(t, "reset-counter", sub.Name(),
			"reset-counter subcommand must not be registered (got %s)", sub.Use)
	}
}

// TestProjects_MergeReportsShortIDExtensions exercises the auto-extension
// reporting path on `kata projects merge`. The two issues share the
// length-4 short_id `d4ex`, so the merge extends the source-side row to
// length 5 (`xd4ex`) before moving it onto the target.
func TestProjects_MergeReportsShortIDExtensions(t *testing.T) {
	const (
		// last 4 = D4EX, last 5 = XD4EX → extends to xd4ex.
		srcUID = "01HZNQ7VFPK1XGD8R5MABXD4EX"
		// last 4 = D4EX, last 5 = CD4EX → stays as d4ex on the target.
		dstUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	)
	env := testenv.New(t)
	ctx := context.Background()
	src, err := env.DB.CreateProject(ctx, "src")
	require.NoError(t, err)
	dst, err := env.DB.CreateProject(ctx, "dst")
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: src.ID, Title: "from src", Author: "tester", UID: srcUID,
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: dst.ID, Title: "from dst", Author: "tester", UID: dstUID,
	})
	require.NoError(t, err)

	out, err := runCmdOutput(t, env, "--json", "projects", "merge", itoa(src.ID), itoa(dst.ID))
	require.NoError(t, err)

	var got struct {
		ShortIDExtensions []map[string]string `json:"short_id_extensions"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.ShortIDExtensions, 1)
	assert.Equal(t, "d4ex", got.ShortIDExtensions[0]["pre_merge_short_id"])
	assert.Equal(t, "xd4ex", got.ShortIDExtensions[0]["post_merge_short_id"])
	assert.Equal(t, srcUID, got.ShortIDExtensions[0]["uid"])
}

// TestProjects_MergeHumanOutputReportsExtensions covers the non-JSON path: the
// merged-project summary line drops the legacy `next #N` clause and gains a
// per-extension `extended <project>#<short> from <pre> to <post>` line.
func TestProjects_MergeHumanOutputReportsExtensions(t *testing.T) {
	const (
		srcUID = "01HZNQ7VFPK1XGD8R5MABXD4EX"
		dstUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	)
	env := testenv.New(t)
	ctx := context.Background()
	src, err := env.DB.CreateProject(ctx, "src")
	require.NoError(t, err)
	dst, err := env.DB.CreateProject(ctx, "dst")
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: src.ID, Title: "from src", Author: "tester", UID: srcUID,
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: dst.ID, Title: "from dst", Author: "tester", UID: dstUID,
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "projects", "merge", itoa(src.ID), itoa(dst.ID))
	assert.NotContains(t, out, "next #")
	assert.Contains(t, out, "extended dst#xd4ex from d4ex to xd4ex")
}
