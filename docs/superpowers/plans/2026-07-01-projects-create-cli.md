# Projects Create CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `kata projects create <name>` so users can create or ensure daemon projects without initializing a workspace.

**Architecture:** Reuse the existing daemon `POST /api/v1/projects` path-free name flow. Add a Cobra subcommand in `cmd/kata/projects.go`, render existing human/JSON/agent output modes, and document the command in the CLI reference and quickstart.

**Tech Stack:** Go, Cobra CLI, existing kata daemon HTTP API, existing CLI test helpers.

---

### Task 1: CLI Behavior Tests

**Files:**
- Modify: `cmd/kata/projects_test.go`

- [ ] **Step 1: Write failing tests**

Add tests that exercise the public CLI contract:

```go
func TestProjectsCreateCreatesProjectWithoutWorkspaceBinding(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()

	out := requireCmdOutput(t, env, "--workspace", dir, "projects", "create", "example-project")

	assert.Contains(t, out, "created project #")
	assert.Contains(t, out, "(example-project)")
	assert.NoFileExists(t, filepath.Join(dir, ".kata.toml"))
	got, err := env.DB.ProjectByName(context.Background(), "example-project")
	require.NoError(t, err)
	assert.Equal(t, "example-project", got.Name)
}
```

Add companion tests for existing projects, JSON output, agent output,
whitespace-only names, and archived-name conflicts.

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./cmd/kata -run 'TestProjectsCreate' -count=1
```

Expected: failure because `projects create` is not registered.

### Task 2: CLI Command

**Files:**
- Modify: `cmd/kata/projects.go`

- [ ] **Step 1: Implement the command**

Register `projectsCreateCmd()` in `newProjectsCmd()`. The command should:

```go
name := strings.TrimSpace(strings.Join(args, " "))
if name == "" {
	return &cliError{Message: "project name must be non-empty", Kind: kindValidation, ExitCode: ExitValidation}
}
bs, err := postProjects(ctx, baseURL, map[string]string{"name": name})
```

Decode `project.id`, `project.name`, and `created`. Render:

```text
created project #<id> (<name>)
project #<id> (<name>) already exists
OK project action=create id=<id> project=<name> created=<bool>
```

JSON mode should emit the raw daemon response with `emitJSON`.

- [ ] **Step 2: Run focused tests to verify they pass**

Run:

```bash
go test ./cmd/kata -run 'TestProjectsCreate' -count=1
```

Expected: all `TestProjectsCreate...` tests pass.

### Task 3: Documentation

**Files:**
- Modify: `docs/reference/cli.md`
- Modify: `docs/get-started/quickstart.md`

- [ ] **Step 1: Update CLI docs**

Add `kata projects create <name>` to the project command synopsis and describe
that it creates or returns a daemon project without writing workspace files.

- [ ] **Step 2: Update quickstart wording**

Keep `kata init` as the workspace-binding path, but mention `kata projects
create <name>` for projects that are not tied to a repository workspace.

### Task 4: Verification And Commit

**Files:**
- Verify all modified files.

- [ ] **Step 1: Run focused verification**

Run:

```bash
go test ./cmd/kata ./internal/daemon
```

Expected: pass.

- [ ] **Step 2: Run full Go verification**

Run:

```bash
go test ./...
```

Expected: pass.

- [ ] **Step 3: Commit implementation**

Stage only the relevant code, docs, and plan/spec files, then commit with a
message explaining why name-only project creation belongs in the projects
subcommand instead of overloading `kata init`.
