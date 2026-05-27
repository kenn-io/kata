# `kata ready --all`: global ready view

## Motivation

`kata ready` currently requires a single-project context: it resolves a project
via `--project`, a local `.kata.toml`, or the workspace path, then asks the
daemon for ready issues in that one project. There is no way to ask the daemon
"across every project, what is ready to work on right now?"

This is needed for **agents coordinating work across multiple projects**. An
agent that owns several kata projects (or hops between them) currently has to
enumerate projects out-of-band and call `kata ready` once per project, then
merge the results client-side. A first-class global view collapses that into a
single command and a single daemon query, with consistent ordering and a
meaningful global `--limit`.

## Behavior

### CLI

- `kata ready` (no flag): **unchanged**. Resolves a single project; errors if
  no project context.
- `kata ready --all`: queries all non-archived projects. Skips local project
  resolution entirely.
- `kata ready --project foo --all`: validation error
  (`--project and --all are mutually exclusive`).
- `kata ready --all` from inside a bound `.kata.toml` directory: works; `--all`
  overrides local project scope.
- `--limit N` with `--all`: **global** cap on total rows returned (not
  per-project).

### Output (text mode)

Each row uses the qualified short-ref form documented in `CLAUDE.md`:

```
kata#abc4      Fix the thing            (alice)
other#def7     Wire up the other thing  (-)
```

Per-project (non-`--all`) output is unchanged.

### Output (JSON mode)

The daemon response is emitted as-is. Each issue gains:

- `project_name` — the project's canonical name (string)
- `project_id` — the project's numeric id (int, present via the embedded
  `Issue` row)

### Ordering

Same as today, applied globally: `ORDER BY i.updated_at DESC, i.id DESC`.
Most recently updated ready issues bubble to the top regardless of project,
so projects naturally interleave by recency.

### Archived projects

Excluded. The per-project endpoint already rejects archived projects via
`activeProjectByID` (`internal/daemon/handlers_ready.go:17`); the global query
mirrors that by filtering on `projects.archived_at IS NULL` (or whatever
predicate `activeProjectByID` uses).

## Components

### `cmd/kata/ready.go`

- Add `--all` bool flag.
- When `--all` is set:
  - If `--project` is also set, return a `kindValidation` `cliError` before
    any daemon work.
  - Skip `resolveProjectID` and the workspace start-path resolution.
  - Call `GET /api/v1/ready` instead of `/api/v1/projects/{id}/ready`.
  - In text mode, format each row as
    `<project>#<short_id>  <title>  (<owner>)`.

### `internal/daemon/handlers_ready.go`

- Register a second handler: `GET /api/v1/ready`.
- Calls a new DB method `ReadyIssuesGlobal(ctx, limit)`.
- Response shape mirrors `ReadyResponse` but each issue carries
  `project` (name) and `project_id`.

### `internal/db/queries.go`

- New `ReadyIssuesGlobal(ctx, limit int) ([]ReadyGlobalIssue, error)` (or
  similar — matches whatever return type the new response uses).
- Same readiness rule as `ReadyIssues`: `i.status = 'open' AND i.deleted_at IS
  NULL` and no open `blocks` predecessor.
- Joins `projects` to attach the project name, and filters out archived
  projects.
- Same `ORDER BY i.updated_at DESC, i.id DESC LIMIT ?`.

### `internal/api/types.go`

- New request/response types for the global endpoint. Each returned issue
  carries `project` and `project_id` fields in addition to the existing
  per-issue fields.

## Validation (client-side, before daemon call)

- `--limit` must be ≥ 0 (existing rule, unchanged).
- `--project` and `--all` are mutually exclusive.

## Testing

- `cmd/kata/ready_test.go`: `--all` happy path (text + JSON), `--all +
  --project` validation error, `--all` from a bound `.kata.toml` directory
  succeeds without resolving the local project.
- `internal/daemon/handlers_ready_test.go`: `GET /api/v1/ready` returning
  issues from multiple projects, ordering across projects, archived-project
  exclusion, `limit` as a global cap.
- `internal/db/queries_ready_test.go`: `ReadyIssuesGlobal` covering the same
  readiness rules already tested for the per-project query, plus
  archived-project exclusion and the join to project name.

## Out of scope

- Other `kata` commands (`list`, `search`, `show`) gaining global modes.
- Filter flags shown in the installed binary's help (`--label`, `--owner`,
  `--unowned`, `--no-label`); they are not present in this worktree's source
  and adding them is independent of the global expansion.
- Auth/permissions for cross-project visibility — kata is single-tenant today.
