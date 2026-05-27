# `kata ready --all` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `--all` flag to `kata ready` that lists ready issues across all non-archived projects, for agents coordinating work across multiple kata projects.

**Architecture:** New non-project-scoped daemon endpoint `GET /api/v1/ready` backed by a new DB method `ReadyIssuesGlobal`. The CLI gains a mutually-exclusive `--all` flag that skips local project resolution and calls the new endpoint. Each row in the global response carries the project name; text output uses the qualified short-ref form `<project>#<short_id>`.

**Tech Stack:** Go, huma/v2 (HTTP API framework), SQLite, cobra (CLI), stretchr/testify.

**Spec:** `docs/superpowers/specs/2026-05-27-kata-ready-global-design.md`

---

## File Structure

**Modify:**
- `internal/api/types.go` — add `ReadyGlobalRequest`, `ReadyGlobalResponse`, `ReadyGlobalIssue`.
- `internal/db/queries.go` — add `ReadyIssuesGlobal` near existing `ReadyIssues` (line 1403).
- `internal/db/queries_ready_test.go` — add tests for `ReadyIssuesGlobal`.
- `internal/daemon/handlers_ready.go` — register a second handler for `GET /api/v1/ready`.
- `internal/daemon/handlers_ready_test.go` — add tests for the global endpoint.
- `cmd/kata/ready.go` — add `--all` flag, mutual-exclusion check, alternate URL path, qualified text formatting.
- `cmd/kata/ready_test.go` — add tests for `--all` text output, JSON output, validation error, bound-dir override.

No new files. All changes co-located with existing per-project ready code.

---

## Task 1: Add DB type and method for global ready

**Files:**
- Modify: `internal/db/types.go` (add type near `Issue`)
- Modify: `internal/db/queries.go:1403` (add method below existing `ReadyIssues`)
- Test: `internal/db/queries_ready_test.go` (extend)

- [ ] **Step 1: Write failing test for cross-project ready**

Append to `internal/db/queries_ready_test.go`:

```go
func TestReadyIssuesGlobal_ReturnsIssuesAcrossProjects(t *testing.T) {
	d, ctx, p1 := setupTestProject(t)
	p2, err := d.CreateProject(ctx, "second-project")
	require.NoError(t, err)

	a := makeIssue(t, ctx, d, p1.ID, "in p1", "tester")
	b := makeIssue(t, ctx, d, p2.ID, "in p2", "tester")

	rows, err := d.ReadyIssuesGlobal(ctx, 0)
	require.NoError(t, err)

	got := map[string]string{}
	for _, r := range rows {
		got[r.Issue.ShortID] = r.ProjectName
	}
	assert.Equal(t, p1.Name, got[a.ShortID])
	assert.Equal(t, "second-project", got[b.ShortID])
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/db/ -run TestReadyIssuesGlobal_ReturnsIssuesAcrossProjects -v`
Expected: FAIL with `d.ReadyIssuesGlobal undefined` and/or `db.ReadyIssuesGlobal not found`.

- [ ] **Step 3: Add the `ReadyGlobalIssue` type**

In `internal/db/types.go`, add immediately after the `Issue` struct (after line 101):

```go
// ReadyGlobalIssue is an Issue paired with its project's canonical name. Used
// only by ReadyIssuesGlobal so the cross-project ready view can render
// qualified refs (`<project>#<short_id>`) without a follow-up project lookup.
type ReadyGlobalIssue struct {
	Issue
	ProjectName string `json:"project_name"`
}
```

- [ ] **Step 4: Add the `ReadyIssuesGlobal` method**

In `internal/db/queries.go`, immediately after `ReadyIssues` (after the closing brace at line 1431), add:

```go
// ReadyIssuesGlobal returns ready issues across every non-archived project,
// each paired with its project name. "Ready" matches ReadyIssues: open,
// not soft-deleted, and not blocked by an open `blocks` predecessor.
// Issues from archived projects (projects.deleted_at IS NOT NULL) are
// excluded. Ordering matches ReadyIssues so behavior is consistent.
func (d *DB) ReadyIssuesGlobal(ctx context.Context, limit int) ([]ReadyGlobalIssue, error) {
	q := issueSelect + `, p.name AS project_name
		WHERE i.status = 'open' AND i.deleted_at IS NULL
		  AND p.deleted_at IS NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM links l
		    JOIN issues blocker ON blocker.id = l.from_issue_id
		    WHERE l.type = 'blocks' AND l.to_issue_id = i.id
		      AND blocker.status = 'open' AND blocker.deleted_at IS NULL
		  )
		ORDER BY i.updated_at DESC, i.id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ready issues global: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReadyGlobalIssue
	for rows.Next() {
		var r ReadyGlobalIssue
		if err := rows.Scan(
			&r.Issue.ID, &r.Issue.UID, &r.Issue.ProjectID, &r.Issue.ProjectUID,
			&r.Issue.ShortID, &r.Issue.Title, &r.Issue.Body, &r.Issue.Status,
			&r.Issue.ClosedReason, &r.Issue.Owner, &r.Issue.Priority, &r.Issue.Author,
			&r.Issue.Metadata, &r.Issue.Revision, &r.Issue.RecurrenceID, &r.Issue.OccurrenceKey,
			&r.Issue.CreatedAt, &r.Issue.UpdatedAt, &r.Issue.ClosedAt, &r.Issue.DeletedAt,
			&r.ProjectName,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

Note: this query selects all columns of `issueSelect` plus `p.name AS project_name`, so we cannot reuse `scanIssue` (which expects exactly the `issueSelect` column list). Hand-scan the row into `ReadyGlobalIssue`.

- [ ] **Step 5: Run the test and verify it passes**

Run: `go test ./internal/db/ -run TestReadyIssuesGlobal_ReturnsIssuesAcrossProjects -v`
Expected: PASS.

- [ ] **Step 6: Add test for archived-project exclusion**

Append to `internal/db/queries_ready_test.go`:

```go
func TestReadyIssuesGlobal_ExcludesArchivedProjects(t *testing.T) {
	d, ctx, p1 := setupTestProject(t)
	p2, err := d.CreateProject(ctx, "to-archive")
	require.NoError(t, err)

	keep := makeIssue(t, ctx, d, p1.ID, "keep", "tester")
	hidden := makeIssue(t, ctx, d, p2.ID, "hidden", "tester")

	require.NoError(t, d.RemoveProject(ctx, p2.ID))

	rows, err := d.ReadyIssuesGlobal(ctx, 0)
	require.NoError(t, err)

	got := map[string]bool{}
	for _, r := range rows {
		got[r.Issue.ShortID] = true
	}
	assert.True(t, got[keep.ShortID], "issue in active project is returned")
	assert.False(t, got[hidden.ShortID], "issue in archived project is excluded")
}
```

- [ ] **Step 7: Run the test and verify it passes**

Run: `go test ./internal/db/ -run TestReadyIssuesGlobal_ExcludesArchivedProjects -v`
Expected: PASS.

If `RemoveProject` is not the right API, search for the archive helper:
`grep -rn "func (d \*DB) .*Project" internal/db/queries_projects_remove.go`
Use whatever method sets `projects.deleted_at`.

- [ ] **Step 8: Add test for blocked-issue exclusion (parity with per-project)**

Append to `internal/db/queries_ready_test.go`:

```go
func TestReadyIssuesGlobal_ExcludesBlockedIssues(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	blocker := makeIssue(t, ctx, d, p.ID, "blocker", "tester")
	blocked := makeIssue(t, ctx, d, p.ID, "blocked", "tester")
	makeLink(ctx, t, d, p.ID, blocker.ID, blocked.ID, "blocks")

	rows, err := d.ReadyIssuesGlobal(ctx, 0)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Issue.ShortID] = true
	}
	assert.True(t, got[blocker.ShortID])
	assert.False(t, got[blocked.ShortID])
}
```

- [ ] **Step 9: Run all DB ready tests**

Run: `go test ./internal/db/ -run TestReadyIssues -v`
Expected: all PASS (existing per-project tests + three new global tests).

- [ ] **Step 10: Commit**

```bash
git add internal/db/types.go internal/db/queries.go internal/db/queries_ready_test.go
git commit -m "db: add ReadyIssuesGlobal for cross-project ready view"
```

---

## Task 2: Add API request/response types

**Files:**
- Modify: `internal/api/types.go:639-650` (add new types after `ReadyResponse`)

- [ ] **Step 1: Add the global request/response types**

In `internal/api/types.go`, immediately after the existing `ReadyResponse` (after line 650), add:

```go
// ReadyGlobalRequest is GET /api/v1/ready (no project_id; spans every
// non-archived project).
type ReadyGlobalRequest struct {
	Limit int `query:"limit,omitempty"`
}

// ReadyGlobalResponse is the cross-project ready-issue list. Each row
// carries the project name so clients can render qualified refs
// (`<project>#<short_id>`) without a separate lookup.
type ReadyGlobalResponse struct {
	Body struct {
		Issues []db.ReadyGlobalIssue `json:"issues"`
	}
}
```

- [ ] **Step 2: Verify the package compiles**

Run: `go build ./internal/api/...`
Expected: no output (success).

- [ ] **Step 3: Commit**

```bash
git add internal/api/types.go
git commit -m "api: add ReadyGlobalRequest/Response types"
```

---

## Task 3: Register the daemon `GET /api/v1/ready` handler

**Files:**
- Modify: `internal/daemon/handlers_ready.go`
- Test: `internal/daemon/handlers_ready_test.go` (extend)

- [ ] **Step 1: Write failing test for the global endpoint**

Append to `internal/daemon/handlers_ready_test.go`:

```go
// readyGlobalResp narrows the global response body to the fields these tests
// assert on.
type readyGlobalResp struct {
	Issues []struct {
		ShortID     string `json:"short_id"`
		ProjectName string `json:"project_name"`
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
	pid2 := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	createIssueViaHTTP(t, env, pid2, "from second project")

	out := getReadyGlobal(t, env, "")

	projects := map[string]bool{}
	for _, i := range out.Issues {
		projects[i.ProjectName] = true
	}
	assert.GreaterOrEqual(t, len(projects), 2,
		"global ready returns issues from at least two projects, got %v", projects)
	_ = pid1 // referenced for setup side-effects
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/daemon/ -run TestReadyGlobal -v`
Expected: FAIL — `/api/v1/ready` returns 404 (route not registered).

- [ ] **Step 3: Register the handler**

In `internal/daemon/handlers_ready.go`, replace the existing `registerReadyHandlers` body so it registers both routes:

```go
func registerReadyHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "readyIssues",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/ready",
	}, func(ctx context.Context, in *api.ReadyRequest) (*api.ReadyResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		issues, err := cfg.DB.ReadyIssues(ctx, in.ProjectID, in.Limit)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.ReadyResponse{}
		out.Body.Issues = issues
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "readyIssuesGlobal",
		Method:      "GET",
		Path:        "/api/v1/ready",
	}, func(ctx context.Context, in *api.ReadyGlobalRequest) (*api.ReadyGlobalResponse, error) {
		issues, err := cfg.DB.ReadyIssuesGlobal(ctx, in.Limit)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.ReadyGlobalResponse{}
		out.Body.Issues = issues
		return out, nil
	})
}
```

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestReadyGlobal_ReturnsIssuesFromAllProjects -v`
Expected: PASS.

- [ ] **Step 5: Add test for archived-project exclusion at the HTTP layer**

Append to `internal/daemon/handlers_ready_test.go`:

```go
func TestReadyGlobal_ExcludesArchivedProjects(t *testing.T) {
	env := testenv.New(t)
	pid1, _, _ := setupTwoIssues(t, env)
	pid2 := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	createIssueViaHTTP(t, env, pid2, "doomed")

	// Archive pid2. Use whatever helper or HTTP DELETE the test suite uses for
	// archiving; check existing tests in this package for the pattern, e.g.:
	//   archiveProjectViaHTTP(t, env, pid2)
	// or a direct DELETE on /api/v1/projects/{id}.
	archiveProjectViaHTTP(t, env, pid2)

	out := getReadyGlobal(t, env, "")
	for _, i := range out.Issues {
		assert.NotEqual(t, pid2Name(t, env, pid2), i.ProjectName,
			"archived project's issues must not appear in /api/v1/ready")
	}
	_ = pid1
}
```

If `archiveProjectViaHTTP` / `pid2Name` helpers don't exist, search for the archive pattern used elsewhere in this package and adapt:
`grep -rn "archive\|RemoveProject\|DELETE.*projects" internal/daemon/*_test.go`
Inline whatever is needed; do not invent new test helpers unless they're trivial wrappers.

- [ ] **Step 6: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestReadyGlobal_ExcludesArchivedProjects -v`
Expected: PASS.

- [ ] **Step 7: Add test for `limit` as a global cap**

Append to `internal/daemon/handlers_ready_test.go`:

```go
func TestReadyGlobal_LimitCapsTotalRows(t *testing.T) {
	env := testenv.New(t)
	pid1 := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	pid2 := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/other.git")
	for i := 0; i < 3; i++ {
		createIssueViaHTTP(t, env, pid1, "p1")
		createIssueViaHTTP(t, env, pid2, "p2")
	}

	out := getReadyGlobal(t, env, "?limit=2")
	assert.Len(t, out.Issues, 2, "limit caps total rows across projects, not per-project")
}
```

If both projects can't be initialized in a single test env (depends on workspace setup), use `setupTwoIssues` for the first project and `initWorkspaceViaHTTP` for the second, mirroring `TestReadyGlobal_ReturnsIssuesFromAllProjects`. The point is two projects, six total ready issues, expect exactly two back.

- [ ] **Step 8: Run the test and verify it passes**

Run: `go test ./internal/daemon/ -run TestReadyGlobal_LimitCapsTotalRows -v`
Expected: PASS.

- [ ] **Step 9: Run all daemon ready tests**

Run: `go test ./internal/daemon/ -run TestReady -v`
Expected: all PASS (existing per-project tests + three new global tests).

- [ ] **Step 10: Commit**

```bash
git add internal/daemon/handlers_ready.go internal/daemon/handlers_ready_test.go
git commit -m "daemon: register GET /api/v1/ready for cross-project ready"
```

---

## Task 4: Add `--all` flag to `kata ready` CLI

**Files:**
- Modify: `cmd/kata/ready.go`
- Test: `cmd/kata/ready_test.go` (extend)

- [ ] **Step 1: Write failing test for `--all` text output**

Append to `cmd/kata/ready_test.go`:

```go
func TestReady_AllFlagListsAcrossProjects(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "in-bound-project")

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all")
	require.NoError(t, err)
	// Text rows must use the qualified short-ref form: "<project>#<short_id>".
	// We don't pin the project name (depends on setupCLIWorkspace), but the
	// "#" separator is the contract.
	assert.Contains(t, out, "#",
		"--all output uses qualified refs (project#short_id), got: %q", out)
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./cmd/kata/ -run TestReady_AllFlagListsAcrossProjects -v`
Expected: FAIL — `unknown flag: --all`.

- [ ] **Step 3: Implement `--all` in `cmd/kata/ready.go`**

Replace the contents of `cmd/kata/ready.go` with:

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

func newReadyCmd() *cobra.Command {
	var (
		limit int
		all   bool
	)
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "list open issues with no open blocks predecessor",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 0 {
				return &cliError{Message: "--limit must be non-negative", Kind: kindValidation, ExitCode: ExitValidation}
			}
			if all && flags.Project != "" {
				return &cliError{
					Message:  "--project and --all are mutually exclusive",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}

			var getURL string
			if all {
				getURL = baseURL + "/api/v1/ready"
			} else {
				start, err := resolveStartPath(flags.Workspace)
				if err != nil {
					return err
				}
				pid, err := resolveProjectID(ctx, baseURL, start)
				if err != nil {
					return err
				}
				getURL = fmt.Sprintf("%s/api/v1/projects/%d/ready", baseURL, pid)
			}
			if limit > 0 {
				getURL += fmt.Sprintf("?limit=%d", limit)
			}

			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, getURL, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}

			if all {
				var b struct {
					Issues []struct {
						ShortID     string  `json:"short_id"`
						Title       string  `json:"title"`
						Owner       *string `json:"owner,omitempty"`
						ProjectName string  `json:"project_name"`
					} `json:"issues"`
				}
				if err := json.Unmarshal(bs, &b); err != nil {
					return err
				}
				for _, i := range b.Issues {
					owner := "-"
					if i.Owner != nil {
						owner = *i.Owner
					}
					qualified := i.ProjectName + "#" + i.ShortID
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-16s  %s  (%s)\n",
						qualified, textsafe.Line(i.Title), textsafe.Line(owner)); err != nil {
						return err
					}
				}
				return nil
			}

			var b struct {
				Issues []struct {
					ShortID string  `json:"short_id"`
					Title   string  `json:"title"`
					Owner   *string `json:"owner,omitempty"`
				} `json:"issues"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			for _, i := range b.Issues {
				owner := "-"
				if i.Owner != nil {
					owner = *i.Owner
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-8s  %s  (%s)\n",
					i.ShortID, textsafe.Line(i.Title), textsafe.Line(owner)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = no limit)")
	cmd.Flags().BoolVar(&all, "all", false, "list ready issues across all non-archived projects")
	return cmd
}
```

`flags.Project` is the existing global `--project` value (used the same way in `cmd/kata/create.go:327`, `cmd/kata/events.go:42`, `cmd/kata/digest.go:44`).

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./cmd/kata/ -run TestReady_AllFlagListsAcrossProjects -v`
Expected: PASS.

- [ ] **Step 5: Add test for JSON output carrying `project_name`**

Append to `cmd/kata/ready_test.go`:

```go
func TestReady_AllFlagJSONIncludesProjectName(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "first")

	out, err := runCmdOutput(t, env, "--workspace", dir, "--json", "ready", "--all")
	require.NoError(t, err)
	var got struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotEmpty(t, got.Issues)
	first := got.Issues[0]
	_, hasProject := first["project_name"]
	assert.True(t, hasProject, "project_name missing from --all JSON row: %v", first)
}
```

- [ ] **Step 6: Run the test and verify it passes**

Run: `go test ./cmd/kata/ -run TestReady_AllFlagJSONIncludesProjectName -v`
Expected: PASS.

- [ ] **Step 7: Add test for `--project + --all` validation error**

Append to `cmd/kata/ready_test.go`:

```go
func TestReady_AllAndProjectAreMutuallyExclusive(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	_, err := runCmdOutput(t, env, "--workspace", dir,
		"--project", "anything", "ready", "--all")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}
```

- [ ] **Step 8: Run the test and verify it passes**

Run: `go test ./cmd/kata/ -run TestReady_AllAndProjectAreMutuallyExclusive -v`
Expected: PASS.

- [ ] **Step 9: Add test for `--all` from a bound directory**

Append to `cmd/kata/ready_test.go`:

```go
// TestReady_AllFromBoundDirSkipsLocalProject pins that --all does not require
// (or use) the local .kata.toml project context: an agent in a bound workspace
// can still get the global view.
func TestReady_AllFromBoundDirSkipsLocalProject(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	// dir has a .kata.toml from setup; createIssue is project-scoped so the
	// bound project has at least one ready issue.
	createIssue(t, env, pid, "from-bound-project")

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, "#",
		"--all from bound dir still emits qualified refs, got: %q", out)
}
```

- [ ] **Step 10: Run the test and verify it passes**

Run: `go test ./cmd/kata/ -run TestReady_AllFromBoundDirSkipsLocalProject -v`
Expected: PASS.

- [ ] **Step 11: Run all CLI ready tests**

Run: `go test ./cmd/kata/ -run TestReady -v`
Expected: all PASS (existing per-project tests + four new --all tests).

- [ ] **Step 12: Commit**

```bash
git add cmd/kata/ready.go cmd/kata/ready_test.go
git commit -m "cli: add kata ready --all for cross-project ready view"
```

---

## Task 5: Final verification

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`
Expected: all PASS, no skips.

- [ ] **Step 2: Spot-check the binary**

Run: `go build -o /tmp/kata ./cmd/kata && /tmp/kata ready --help`
Expected output includes a line like:

```
      --all                list ready issues across all non-archived projects
```

- [ ] **Step 3: No commit (verification-only task)**

If everything passes, the feature is complete. Open issues, if any, should go to a follow-up.
