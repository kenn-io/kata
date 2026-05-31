# Contributing

kata is a Go project with a local daemon, CLI, TUI, SQLite store, JSONL
import/export path, and federation tests. Keep changes small, verified, and
documented.

## Repository layout

| Path | Responsibility |
| --- | --- |
| `cmd/kata` | CLI commands and output modes. |
| `internal/daemon` | HTTP routes, daemon runtime, auth, SSE, federation routes. |
| `internal/db` | SQLite schema, projections, events, queries, federation state. |
| `internal/client` | Client discovery, auto-start, remote daemon, bearer handling. |
| `internal/tui` | Bubble Tea TUI. |
| `internal/jsonl` | Export/import, cutover, fixture compatibility. |
| `internal/federation` | Spoke-side federation client and runner. |
| `docs` | Public Zensical documentation source, design notes, and unpublished historical specs. |

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
`docs/design/` can cover deeper implementation context. Historical design specs
under `docs/superpowers/specs` are useful for maintainers, but they should not
be published wholesale because some decisions have changed during
implementation.

When changing behavior:

- update CLI help when flags or contracts change;
- update `README.md` if the project overview or quickstart changes;
- update `docs/` for public user/operator behavior;
- keep historical planning context under `docs/superpowers/` only when it helps
  maintainers.

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
