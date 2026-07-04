# Contributing

kata is a Go project with a local daemon, CLI, TUI, SQLite store, JSONL
import/export path, and federation tests. Keep changes small, verified, and
documented.

## Repository layout

| Path | Responsibility |
| --- | --- |
| `cmd/kata` | CLI commands and output modes. |
| `internal/daemon` | HTTP routes, daemon runtime, auth, SSE, federation routes. |
| `internal/db` | Storage contract, backend implementations, schemas, projections, events, queries, federation state. |
| `internal/client` | Client discovery, auto-start, remote daemon, bearer handling. |
| `internal/tui` | Bubble Tea TUI. |
| `internal/jsonl` | Export/import, cutover, fixture compatibility. |
| `internal/federation` | Spoke-side federation client and runner. |
| `docs` | Public Zensical documentation source and maintained design notes. |

## Local checks

Run:

```sh
make test
make vet
make lint
make nilaway
```

Federation-specific checks:

```sh
make test-stress
make test-federation-docker
```

`make test-stress` runs randomized and failpoint tests. If Rapid prints a
failing seed, reproduce it with the seed from the failure output:

```sh
RAPID_SEED=<seed> go test -tags federation_stress ./e2e \
  -run TestFederationStressRandomizedWorkload \
  -count=1 \
  -timeout 2m
```

## Testing standards

Tests should protect behavior that users, operators, or callers can observe.
Prefer assertions against returned values, persisted rows, HTTP response bodies
and status codes, emitted events, CLI output and exit codes, rendered TUI
output, or auth outcomes. Avoid tests that only prove the current source text
contains a string or that a private implementation detail is arranged the way it
is today.

For shell scripts, workflows, config, and generated artifacts, do not add tests
that grep implementation files for expected text. Those content checks are
usually tautological: they pass when the file contains the searched string, not
when the release, installer, workflow, or daemon behavior works. Exercise the
command or tool behavior directly, use the tool's own validation command, or
document a manual check when direct behavior coverage is not practical.

When a fake stands in for an external boundary such as a hub, GitHub, or an
embedding provider, keep it specific enough to catch the wrong branch,
argument, status, or side effect. Do not mock the subject under test, and do
not update golden snapshots or expected literals just to silence a failure
without first confirming that the new output is the intended contract.

## Documentation checks

Install Zensical:

```sh
make docs-install
```

Build the site:

```sh
make docs-check
```

Preview locally:

```sh
make docs-serve
```

Zensical's preview server is for local preview only. Publish the generated
static files from `site/` with a real static host, CDN, or web server.

## Documentation standards

Public docs should describe implemented behavior first. Technical notes under
`docs/design/` cover deeper design rationale, trade-offs, and low-level
constraints; keep them current as decisions change rather than letting stale
design drafts accumulate.

When changing behavior:

- update CLI help when flags or contracts change;
- update `README.md` if the project overview or quickstart changes;
- update `docs/` for public user/operator behavior;
- record durable design rationale in `docs/design/` when the "why" is not
  obvious from the code.

## Commit discipline

Do not leave accepted repository changes uncommitted at the end of a task.
Do not squash or amend history unless explicitly asked.

When closing kata issues, close with a substantive message and typed evidence:

```sh
kata close abc4 --done \
  --message "Updated docs for remote daemon auth and verified docs-check passes." \
  --commit <sha> \
  --test "make docs-check"
```

If work is incomplete, leave the issue open and add a comment explaining what
was attempted and what remains.
