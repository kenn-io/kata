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
| `internal/embedding` | OpenAI-compatible embeddings client and text recipe (storage-free). |
| `internal/vector` | Backend-native semantic index: SQLite sidecar/sqlite-vec or canonical PostgreSQL/pgvector tables, plus mirror, generation lifecycle, fill, and query. |
| `internal/client` | Client discovery, auto-start, remote daemon, bearer handling. |
| `internal/tui` | Bubble Tea TUI. |
| `web` | Svelte browser source, generated API types, component tests, Playwright fixtures, and asset tooling. |
| `internal/jsonl` | Export/import, cutover, fixture compatibility. |
| `internal/federation` | Spoke-side federation client and runner. |
| `docs` | Public Zensical documentation source and maintained design notes. |

## Building

Release binaries are pure Go (`CGO_ENABLED=0`): `modernc.org/sqlite` provides
both the database driver and the sqlite-vec extension. Development builds on
machines with a C toolchain default to `CGO_ENABLED=1`, where kit's sqlitevec
selects its cgo sqlite-vec bindings by build constraint; those need C sqlite
symbols at link time, so `internal/vector/driver_cgo.go` links
`mattn/go-sqlite3` purely to keep ambient `go build`/`go test` working. Both
driver shims live in `internal/vector/driver_*.go`; nothing to configure,
but don't remove the mattn dependency because release builds don't use it.

## Local checks

Install the pinned Go and lint tools, then run the checks:

```sh
mise install
make test
make vet
make lint
make nilaway
```

### Browser UI development

The web toolchain is pinned to Node `26.5.1` and Bun `1.3.14`; use those exact
versions so the frozen lockfile and generated output match CI.

Install the pinned web dependencies, then start the current Go checkout and
Vite together:

```sh
make web-install
make web-dev
```

`web-dev` builds the repository binary without installing it and creates an
owned temporary home, workspace, and database beneath the ignored
`.kata-web-dev/` directory. Vite serves `http://127.0.0.1:5173` and proxies API
and event-stream requests to that isolated daemon using the same configured
origin, browser-session, CSRF, and SSE policy as production. It does not add a
development authentication or Origin bypass. Set `KATA_WEB_DEV_PORT` to use a
different Vite port.

Run the static, unit, and real proxy checks with:

```sh
make web-check web-test
make web-test-browser
make web-e2e
```

The browser check creates a real session inside its isolated run; it
does not use an installed daemon or a developer database.

Before release packaging, run:

```sh
make web-build web-assets-check web-embed
make web-release-check
```

`web-release-check` builds a full Kata binary and refuses the harmless Go-only
asset stub or an incomplete production graph. Published archives must all use
the same validated embedded distribution.

Federation-specific checks:

```sh
make test-stress
make test-federation-docker
```

The pre-commit hook protects released PostgreSQL migration history. Install
the configured hooks with `prek install`; see [PostgreSQL
migrations](postgres-migrations.md) before proposing a schema change.

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

Write for the person trying to use or maintain Kata:

- Lead with the outcome, name who does what, use short sentences, and explain
  unfamiliar terms.
- Organize around reader questions. Put purpose and current capabilities
  first; separate limitations and future work. Use only the sections needed.
- Give each bullet one main idea. Use numbered steps for sequences,
  paragraphs for reasons, and tables or diagrams when they clarify a flow or
  comparison.
- State rules directly. Preserve exact commands, field names, authorization
  checks, limits, and failure behavior when simplifying the wording.
- Give each fact an owning guide or reference and link to it elsewhere.
  Update that section instead of appending a history of the latest change.
  Indexes should route readers to the details.
- Describe current architecture separately from approved but unbuilt work,
  proposals, and historical decisions. Preserve rationale, approvals, and
  active exceptions with their removal conditions. Label superseded designs
  and keep them outside normal navigation.
- Keep the website, its Markdown companions, README, and documentation in
  agreement. Distinguish the latest release from newer `main` functionality.

Technical notes under `docs/design/` explain design decisions and constraints.
Keep them current as decisions change.

### Release notes

Lead each entry with what the reader can do or what now works. Explain the
problem before naming an internal component. For example, write "Read issue
titles while editing in a narrow pane" instead of "Improve responsive editor
layout."

Check claims against the tagged source. Include required upgrade steps and
link to the owning guide for setup and detailed rules. Release versions and
daemon API versions are separate; preserve the exact compatibility requirement.
Verify publication and downloads before describing a tag as the latest
available release. Credit contributors using the commits and merged pull
requests since the previous release.

When simplifying an older changelog, preserve its release date, behavior at
that version, limits, and contributor credits. Do not describe a later feature
as part of the older release.

### Pages and publication

`docs/website/` owns the product website and its Markdown companions. Zensical
renders the reference site from the Markdown pages listed in
`docs/zensical.toml`. The build combines both into `docs/site/`: the website at
`/`, the introductory guide at `/guide/`, and the documentation at `/docs/`.
Keep `docs/llms.txt` links aligned with those public routes.

When changing behavior:

- update CLI help when flags or contracts change;
- update `README.md` if the project overview or quickstart changes;
- update `docs/` for public user/operator behavior;
- record durable design rationale in `docs/design/` when the "why" is not
  obvious from the code.

Run the [documentation checks](#documentation-checks) before committing.
Use the Makefile's publication commands and follow
[Deploying docs](deploying-docs.md) when publishing.

### Page frontmatter

Every Markdown page under `docs/guide/`, `docs/operations/`, and
`docs/reference/`, including subdirectories, must start with YAML frontmatter.
`README.md` files are exempt. Use this format:

```yaml
---
title: Configuration reference
description: "Configure Kata's daemon and clients."
last_edited: 2026-09-15
---
```

- Set `title` to the page heading and `description` to a short sentence about
  what the reader can learn or do. Both must be non-empty strings.
- Set `last_edited` to the creation date in `YYYY-MM-DD` format. Update it when
  the page content changes; do not bump dates on unchanged pages.
- Run `make docs-check` before submitting. It checks these required fields
  with `docs/scripts/check_frontmatter.py`.

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
