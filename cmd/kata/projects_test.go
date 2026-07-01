package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestProjects_ListAgentIncludesStats(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "kata")
	require.NoError(t, err)
	open, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "open", Author: "tester"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "closed", Author: "tester"})
	require.NoError(t, err)
	_, _, _, err = env.DB.CloseIssue(ctx, open.ID, "done", "tester", "", nil)
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "--agent", "projects", "list")

	assert.Equal(t, "OK projects count=1\n- project=kata id="+itoa(p.ID)+" open=1 closed=1\n", out)
}

func TestProjectsCreateCreatesProjectWithoutWorkspaceBinding(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()

	out := requireCmdOutput(t, env, "--workspace", dir, "projects", "create", "example-project")

	assert.Contains(t, out, "created project #")
	assert.Contains(t, out, "(example-project)")
	assert.NoFileExists(t, filepath.Join(dir, ".kata.toml"))
	assert.NoFileExists(t, filepath.Join(dir, ".gitignore"))
	got, err := env.DB.ProjectByName(context.Background(), "example-project")
	require.NoError(t, err)
	assert.Equal(t, "example-project", got.Name)
}

func TestProjectsCreateExistingProjectIsIdempotent(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "projects", "create", "example-project")

	assert.Equal(t, "project #"+itoa(p.ID)+" (example-project) already exists\n", out)
}

func TestProjectsCreateJSONReturnsDaemonResponse(t *testing.T) {
	env := testenv.New(t)

	out := requireCmdOutput(t, env, "--json", "projects", "create", "example-project")

	var got struct {
		Project struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.NotZero(t, got.Project.ID)
	assert.Equal(t, "example-project", got.Project.Name)
	assert.True(t, got.Created)
}

func TestProjectsCreateFormatJSONReturnsDaemonResponse(t *testing.T) {
	env := testenv.New(t)

	out := requireCmdOutput(t, env, "--format", "json", "projects", "create", "example-project")

	var got struct {
		Project struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.NotZero(t, got.Project.ID)
	assert.Equal(t, "example-project", got.Project.Name)
	assert.True(t, got.Created)
}

func TestProjectsCreateAgentOutput(t *testing.T) {
	env := testenv.New(t)

	out := requireCmdOutput(t, env, "--agent", "projects", "create", "example-project")

	got, err := env.DB.ProjectByName(context.Background(), "example-project")
	require.NoError(t, err)
	assert.Equal(t, "OK project action=create id="+itoa(got.ID)+" project=example-project created=true\n", out)
}

func TestProjectsCreateRejectsWhitespaceNameBeforeRequest(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	_, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), srv.URL),
		"projects", "create", "   ")

	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "project name must be non-empty")
	assert.Equal(t, 0, requests)
}

func TestProjectsCreatePreservesArchivedNameConflict(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "archived-project")
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	_, err = runCmdOutput(t, env, "projects", "create", "archived-project")

	ce := requireCLIError(t, err, ExitConflict)
	assert.Equal(t, "project_archived", ce.Code)
	assert.Contains(t, ce.Message, "archived")
}

func TestProjects_ShowAgentUsesReadShape(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "kata")
	require.NoError(t, err)
	closed, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "closed", Author: "tester"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "open", Author: "tester"})
	require.NoError(t, err)
	_, _, _, err = env.DB.CloseIssue(ctx, closed.ID, "done", "tester", "", nil)
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "--agent", "projects", "show", itoa(p.ID))

	assert.Equal(t, "OK projects count=1\n- project=kata id="+itoa(p.ID)+"\n", out)
}

func TestProjects_ShowAgentDoesNotFetchStats(t *testing.T) {
	var statsRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/projects" && r.URL.Query().Get("include") == "stats":
			statsRequests++
			http.Error(w, "unexpected stats request", http.StatusTeapot)
		case r.URL.Path == "/api/v1/projects":
			_, _ = w.Write([]byte(`{"projects":[{"id":1,"name":"kata"}]}`))
		case r.URL.Path == "/api/v1/projects/1":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"kata"},"aliases":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	resetFlags(t)
	stdout, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), srv.URL),
		"--agent", "projects", "show", "kata")
	require.NoError(t, err)

	assert.Equal(t, "OK projects count=1\n- project=kata id=1\n", stdout)
	assert.Equal(t, 0, statsRequests)
}

func TestProjects_ShowNumericIDDoesNotListProjects(t *testing.T) {
	var listRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			listRequests++
			http.Error(w, "unexpected list request", http.StatusTeapot)
		case "/api/v1/projects/1":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"kata"},"aliases":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	resetFlags(t)
	stdout, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), srv.URL),
		"--agent", "projects", "show", "1")
	require.NoError(t, err)

	assert.Equal(t, "OK projects count=1\n- project=kata id=1\n", stdout)
	assert.Equal(t, 0, listRequests)
}

func TestProjects_RestoreArchivedProjectByName(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "retry-import")
	require.NoError(t, err)
	_, err = env.DB.AttachAlias(ctx, p.ID, "github.com/wesm/retry-import", "git")
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

func TestProjects_RenameAgentOutput(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "old")
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "--agent", "projects", "rename", itoa(p.ID), "new name")

	assert.Equal(t, "OK project action=rename id="+itoa(p.ID)+" project=\"new name\"\n", out)
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

func TestProjectsRewriteAuthorCLIReportsCounts(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ctx := context.Background()
	from, to := "old-agent", "new-agent"
	owner := from
	subject, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: pid,
		Title:     "rewrite subject",
		Author:    from,
		Owner:     &owner,
	})
	require.NoError(t, err)
	peer, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: pid,
		Title:     "rewrite peer",
		Author:    "peer-agent",
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(ctx, db.CreateCommentParams{
		IssueID: subject.ID,
		Author:  from,
		Body:    "rewrite comment",
	})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: subject.ID,
		ToIssueID:   peer.ID,
		Type:        "blocks",
		Author:      from,
	})
	require.NoError(t, err)

	out := runCLI(t, env, dir, "projects", "rewrite-author",
		"--from", from, "--to", to)

	assert.Contains(t, out, "author identity rewritten")
	assert.Contains(t, out, "issue authors: 1")
	assert.Contains(t, out, "issue owners: 1")
	assert.Contains(t, out, "comment authors: 1")
	assert.Contains(t, out, "link authors: 1")
	updated, err := env.DB.IssueByID(ctx, subject.ID)
	require.NoError(t, err)
	assert.Equal(t, to, updated.Author)
}

func TestProjectsRewriteAuthorCLIJSONCounts(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ctx := context.Background()
	from, to := "old-agent", "new-agent"
	_, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: pid,
		Title:     "rewrite subject",
		Author:    from,
	})
	require.NoError(t, err)

	out := runCLI(t, env, dir, "--json", "projects", "rewrite-author",
		"--from", from, "--to", to)

	var got struct {
		Changed        bool  `json:"changed"`
		IssueAuthors   int64 `json:"issue_authors"`
		IssueOwners    int64 `json:"issue_owners"`
		CommentAuthors int64 `json:"comment_authors"`
		LinkAuthors    int64 `json:"link_authors"`
		Total          int64 `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.True(t, got.Changed)
	assert.Equal(t, int64(1), got.IssueAuthors)
	assert.Equal(t, int64(0), got.IssueOwners)
	assert.Equal(t, int64(0), got.CommentAuthors)
	assert.Equal(t, int64(0), got.LinkAuthors)
	assert.Equal(t, int64(1), got.Total)
}

func TestProjectsRewriteAuthorCLIRejectsEmptyTo(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	_, err := runCLICapture(t, env, dir, "projects", "rewrite-author",
		"--from", "old-agent", "--to", "   ")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--to is required")
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

func TestProjects_PurgeArchivedFreesName(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "projects", "purge", "spoke-project",
		"--force", "--confirm", "PURGE spoke-project")
	assert.Contains(t, out, "purged")
	assert.Contains(t, out, "name is now free")

	_, err = env.DB.ProjectByID(ctx, p.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestProjects_PurgeRequiresForce(t *testing.T) {
	env := testenv.New(t)
	// No project created: the --force gate must fire before any daemon lookup,
	// so the error is the validation error, not a not-found.
	_, err := runCmdOutput(t, env, "projects", "purge", "spoke-project")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "--force")
}

func TestProjects_PurgeNoConfirmNonTTYFails(t *testing.T) {
	env := testenv.New(t)
	// Force non-TTY so resolveConfirm rejects with confirm_required instead of
	// prompting; otherwise a TTY stdin would yield confirm_mismatch.
	stubIsTTY(t, false)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)
	_, err = runCmdOutput(t, env, "projects", "purge", "spoke-project", "--force")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, "confirm_required", ce.Code)
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
