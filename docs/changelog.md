---
title: Changelog
description: Release history for kata
---

All notable changes to kata, grouped by release. Versioned releases start with
0.5.0; earlier entries are a retroactive project history grouped by ISO week.

## Unreleased

**New features**

- Added production Postgres storage selected with `KATA_DSN`, `[storage].dsn`,
  or a `postgres://` / `postgresql://` URL. Postgres now implements the complete
  storage contract, including federation, claims, external import, JSONL
  replay, lexical search, daemon startup, export, and atomic snapshot restore.
- Added `kata storage postgres migrate` and `status`, plus validation-only
  runtime configuration, so production deployments can separate schema-owner
  and serving credentials.
- Added PostgreSQL semantic search with pgvector `halfvec` storage and the same
  generation/fill contract as SQLite, covered end to end against a real
  pgvector service.

**Improvements**

- Runs the same behavioral storage conformance fixtures against SQLite and
  Postgres, with no expected Postgres failures or generated method stubs.
- Added Postgres JSONL restore targets. Fresh restores install the dedicated
  `kata` schema; `--force` atomically replaces existing kata-owned state, and
  failed first restores remove only an unchanged fresh schema.
- Established the first released Postgres schema as the migration floor rather
  than retaining migrations for development-only versions that never shipped.
- Added an append-only numbered migration convention and pre-commit history
  guard for PostgreSQL schema changes after that release floor.
- Required server-identity-verified TLS for every remote Postgres connection
  candidate, with a separately configured lab-only insecure opt-in.
- Added a dedicated PostgreSQL CI service job that cannot silently skip backend
  conformance and operator-ceremony tests when Docker discovery is unavailable.
- Made generic CI lanes skip automatic PostgreSQL testcontainers immediately
  while preserving local autostart and fail-hard coverage in the explicit
  service job.
- Added a real-daemon federation matrix proving bidirectional synchronization,
  durable restart recovery, and lease-authorized edits across every SQLite and
  PostgreSQL hub/spoke pairing.
- Isolated daemon namespaces by PostgreSQL schema, fenced restore against
  daemons on every host, and made failed fresh-import cleanup survive request
  cancellation.

## 0.10.0
<small>2026-07-11</small>

kata 0.10.0 makes ready-work selection priority-aware, carries cross-project
relationships through federation, and improves daemon and agent-workflow
operations.

**New features**

- Added `kata next`, which deterministically selects one ready issue. Explicit
  priorities beat unprioritized work, lower numeric priorities win, and ties
  retain the ready API's order. It supports the scoped `ready` filters,
  cross-project `--all` selection, compact human/agent/JSON output, and
  `--full` issue detail.
- Rendered parent/child trees in human `kata list` output with box-drawing
  connectors while preserving server order in JSON and agent output. Children
  whose parent is outside the fetched result remain visible at the top level.
- Added `kata daemon restart`. It validates replacement settings before
  stopping the current local daemon, waits for graceful shutdown, and starts a
  replacement with configured or explicitly repeated listener settings.
- Synchronized cross-project links across federated projects. Link events are
  retained when a peer has not arrived yet, then materialize after both
  endpoint projects join the same hub federation group regardless of project
  enrollment or synchronization order.
- Added `kata federation quarantine list` and `show` so operators can inspect
  project ownership, event ranges and UIDs, timestamps, and retained errors
  before retrying or skipping. Federation project detail in the TUI now shows
  the same retained quarantine errors.

**Improvements**

- Reported live semantic-search backfill progress through the `/health`
  `embeddings` object, including start and last-progress timestamps plus a
  smoothed processing rate and ETA once enough progress samples exist.
- Improved human `kata daemon status` output with the daemon address, PID,
  binary version, and uptime. JSON status includes the database path and start
  time for programmatic diagnostics.
- Added `kata init --with-hooks` for Claude Code workspaces and moved the
  attention lifecycle logic into the installed `kata` binary. The generated
  exec-form hooks no longer depend on a repository script whose contents could
  change behind an already approved command.
- Extended the managed block written by `kata init --with-agents` with the
  `work.branch`, `work.attention`, and `work.attention_msg` conventions.
  Re-running the command refreshes an older managed block in place.
- Improved `kata wait --timeout` and `--poll-interval` validation: a bare
  number remains rejected as ambiguous, but the error now suggests the
  seconds-qualified spelling and lists supported duration units.

**Bug fixes**

- Fixed a federation deadlock where two projects whose first pending batches
  referenced each other's new issues could both become permanently
  quarantined. Compatible spokes also resend older push quarantines created by
  the former missing-peer validator without advancing the cursor; unrelated
  validation failures remain quarantined.
- Preserved labels in project-scoped and cross-project `kata ready` results by
  returning the same hydrated issue projection used elsewhere in the API.
- Fixed GitHub issue and comment pagination when a `Link` header uses GitHub's
  numeric `/repositories/{id}/...` URL form. kata rewrites that form to the
  bound owner/repository path before applying its credential egress guard.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for parent/child list
  rendering, ready-result labels, duration guidance, `work.*` managed guidance,
  and the binary-backed attention hooks.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for
  federated cross-project link convergence, quarantine recovery and discovery,
  and live embedding progress reporting.
- Thanks to [Wes McKinney](https://github.com/wesm) for priority-aware
  `kata next`, daemon restart, and daemon status improvements.
- Thanks to [Barret Schloerke](https://github.com/schloerke) for numeric GitHub
  pagination URL support.

## 0.9.0
<small>2026-07-09</small>

kata 0.9.0 adds coordination primitives for agent launchers and dashboards,
improves scriptable metadata handling, and refreshes semantic search storage and
terminal rendering.

**New features**

- Added `kata wait`, a read-only fan-out/join command for scripts that need to
  block until one or more issues close or report attention through
  `work.attention`. It supports `--until closed|attention|needs-human|stuck`,
  `--any`/`--all`, polling control, timeouts with a dedicated exit code, and
  JSON output for wrappers.
- Added first-class issue metadata commands and API support. `kata meta set`,
  `kata meta unset`, and `kata meta get` read and patch issue metadata, support
  raw JSON values where needed, and expose optimistic concurrency through
  `--if-match` / `If-Match` revisions.
- Added documented `work.*` metadata conventions for branch orchestration
  workflows, including `work.branch`, `work.attention`, and
  `work.attention_msg`, plus an agent-orchestration runbook for launchers,
  working agents, coordinators, and merge automation.

**Improvements**

- Polished human-readable `kata list` and `kata ready` output with clearer
  status glyphs, priority chips, label chips, owner display, and summary
  footers while preserving machine-readable `--json` and `--agent` output.
- Improved daemon HTTP API performance by gzip-compressing eligible JSON
  responses when clients send `Accept-Encoding: gzip`; server-sent event
  streams stay uncompressed so they keep streaming normally.
- Moved semantic search storage from a single embeddings table in `kata.db`
  to a sidecar vector index built on the shared `kit` vector layer, named
  after the database file (`kata.vectors.db` for the default `kata.db`),
  with chunked embeddings instead of a fixed truncation cap and
  generation-based model swaps: changing `model`, `dims`, or
  `fingerprint_salt` fills a new generation in the background and cuts over
  automatically. During that backfill the vector leg is unavailable — `auto`
  searches degrade to labeled lexical results and explicit
  `semantic`/`hybrid` requests return 503 until the cutover — instead of
  losing the vector index outright; lexical search is unaffected. The
  sidecar is disposable derived state — safe to delete, excluded from
  backups, rebuilt by re-embedding.
- The first daemon start after upgrading re-embeds every issue; the rebuilt
  index activates immediately and serves partial semantic results while the
  backfill drains (the `embeddings` backlog in `/health` reports progress).
  JSONL export no longer carries `issue_embedding` records; import of older
  archives that still contain them skips those records instead of failing.
- Soft-deleting an issue now removes its vectors at the next reconcile, so
  deleted content is never re-sent to the embedding endpoint by later index
  rebuilds. Searches with `include_deleted` rank soft-deleted issues
  lexically only; restoring an issue re-embeds it and semantic recall
  resumes.
- Updated the TUI stack to Charm's v2 Bubble Tea, Bubbles, Lip Gloss, and
  Glamour packages for improved terminal rendering and input behavior.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for `kata wait`, issue
  metadata commands, `work.*` orchestration conventions, and human-readable
  `list` / `ready` polish.
- Thanks to [Wes McKinney](https://github.com/wesm) for gzip API compression,
  semantic search vector storage, and the TUI stack update.

## 0.8.0
<small>2026-07-04</small>

kata 0.8.0 adds a graph-shaped issue API for clients that need to visualize or
analyze connected work, makes daemon projects easier to create without a local
workspace, and records testing guidance for behavior-based validation.

**New features**

- Added `GET /api/v1/projects/{project_id}/issues/{ref}/graph` for reachable
  issue graphs. The endpoint walks parent, `blocks`, and related relationships
  from a source issue, supports `depth=full` or a bounded hop count, can hide
  closed non-source issues with `hide_done=true`, includes cross-project
  `qualified_id` values, and reports unresolved link endpoints without dropping
  the rest of the graph.
- Added `kata projects create <name>` for creating or returning an active
  daemon project by name without writing `.kata.toml`, `.gitignore`, or agent
  guidance files and without attaching a workspace alias.
- Added testing guidance that discourages tautological content assertions,
  especially shell tests that grep scripts or config for implementation text,
  and favors assertions against observable behavior, persisted state, command
  output, API responses, events, or rendered UI.

**Improvements**

- Documented the name-only project creation workflow in quickstart and CLI
  reference docs so projects that are not tied one-to-one to a repository can
  be created before any workspace is initialized.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the
  reachable issue graph API and testing-without-tautologies guidance.
- Thanks to [Wes McKinney](https://github.com/wesm) for name-only project
  creation.

## 0.7.0
<small>2026-06-29</small>

kata 0.7.0 improves discovery, sharing, and release operations. Search can now
use opt-in embeddings for semantic recall while preserving lexical behavior by
default, GitHub sync gains safer daemon-side credentials and parent-link
reconciliation, and federation handles larger adoption and push workloads more
reliably.

**New features**

- Added opt-in semantic search for SQLite-backed projects. Configure an
  OpenAI-compatible `/embeddings` endpoint under `[search.embeddings]` to make
  default `kata search` run hybrid lexical/vector search; use `--lexical`,
  `--hybrid`, or `--semantic` to force a mode. Search remains lexical with no
  embedding config, and automatic fallback reports degraded mode instead of
  silently hiding vector failures.
- Added daemon-scoped GitHub sync credentials. Shared daemons can use
  `[[github_sync.app]]` GitHub App credentials matched by `(host, owner)`,
  env-token fallback is host-scoped with `[github_sync].token_host`, and GitHub
  Enterprise hosts must be explicitly allow-listed.
- Added GitHub sub-issue parent-link synchronization. GitHub-sourced parent
  links are imported and reconciled as source-managed kata parent links, while
  unsupported Enterprise schemas preserve existing source-managed parent links
  instead of deleting them.
- Added `kata projects rewrite-author` and the matching HTTP action for
  project-scoped current-state identity hygiene. It rewrites exact matches in
  issue authors, issue owners, comment authors, and link authors before export,
  sharing, or federation enrollment.
- Added `kata comment edit` and the matching HTTP route. Comment edits preserve
  the comment UID, author, creation time, and thread position, which makes them
  useful for pre-federation content redaction.
- Added `kata projects purge` for permanently deleting archived projects and
  freeing their names, with force/confirmation guards, audit tombstones, JSON
  output, and federation-binding refusal.

**Improvements**

- Improved federation adoption by chunking baseline snapshot pushes so large
  existing projects can be adopted without sending one oversized push request.
- Improved federation push reliability by splitting oversized outbound batches
  and retrying them as smaller batches instead of quarantining only because a
  request was too large.
- Improved `kata update` behavior for development builds. Update checks fetch
  fresh release data, show the latest official release and artifact metadata,
  and require `--force` before replacing a dev build.
- Published release artifacts automatically from tag pushes through the
  release workflow.
- Served nav-listed Markdown documentation sources from docs builds, so public
  `.md` URLs come from the same deployment as the rendered pages.
- Refreshed release documentation and docs navigation for semantic search,
  GitHub sync credentials, project purge, author rewrite, and comment redaction.

**Acknowledgements**

- Thanks to [andy-vdg](https://github.com/andy-vdg) for the scoped GitHub sync
  service credential work and GitHub parent-link synchronization.

## 0.6.0
<small>2026-06-24</small>

kata 0.6.0 expands kata's release and sharing surface: GitHub Issues can now
feed a kata project, private-network daemon deployments have an explicit
tokenless write mode for trusted single-user networks, and Windows users have a
hosted release installer.

**New features**

- Added one-way GitHub issue sync with `kata sync github`, backed by
  daemon-owned bindings, cursors, import mappings, status, and a poller. The
  first provider imports GitHub issues and issue comments through daemon-side
  GitHub credentials, skips pull requests, prefixes imported titles by
  default, and keeps GitHub as the source of truth for synced fields.
- Added provider-neutral issue-sync API/storage foundations so GitHub sync
  state participates in backup, restore, JSONL cutover, and daemon status
  flows. Restored sync bindings come back disabled until explicitly re-enabled
  on the new host.
- Added support for running GitHub sync on federation hubs, so GitHub-origin
  kata events can replicate to spokes while direct GitHub sync on spokes stays
  rejected.
- Added explicitly enabled tokenless writes and event streams for daemons bound
  to literal private IP addresses. Operators can opt in with
  `[auth].allow_unauthenticated_private_network_writes = true` or
  `KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES=1`; token administration
  remains blocked without authentication.
- Added a hosted Windows PowerShell release installer at
  `https://katatracker.com/install.ps1`, including release-asset selection,
  checksum verification, user-local install, and user `Path` updates.

**Improvements**

- Improved local-first federation resilience by preserving relationship writes
  to newly created local issues while their create or snapshot events are still
  pending push to the hub. Once the hub has acknowledged the materializing
  event, later missing-issue responses remain visible errors.
- Tightened the pending-push federation exception to `issue_not_found` errors
  so broader hub route or project misconfiguration is not hidden.
- Changed explicit `kata daemon start` to start a background daemon by default
  and return after startup is confirmed; `kata daemon start --foreground`
  remains the service-manager and hosted-deployment mode.
- Sped up Windows development and release validation by moving broad CLI and
  daemon handler fixtures off slower git/init paths when tests only need a
  seeded project.
- Revamped the README and docs front page into a clearer landing page with
  direct install, quickstart, and feature-orientation paths.
- Expanded the 0.5.0 changelog into a fuller first-release history.

**Bug fixes**

- Restored TUI daemon selection recovery when switching from the daemon
  selector to a daemon with no registered projects. Escape now returns to the
  selector for that switch path while direct empty-daemon startup still shows
  the onboarding state.
- Preserved the federation trust boundary for delayed push scenarios without
  hiding real broken mappings or missing hub state after the pending local
  issue has materialized.

## 0.5.0
<small>2026-06-22</small>

kata 0.5.0 is the first versioned public preview release. It includes the core
local-first issue tracker, a full terminal UI, agent-oriented workflows,
hub-and-spoke federation, portable backup/import paths, and the first release
automation for binary distribution.

**New features**

- Local-first issue tracker backed by a daemon-owned SQLite database, workspace
  bindings, project discovery, project aliases, stable issue ULIDs, and short
  refs derived from those ULIDs.
- Issue lifecycle commands for creating, listing, showing, editing, commenting,
  closing, reopening, deleting, restoring, and purging issues.
- Labels, ownership, assignment, claim/unclaim workflows, parent/child
  hierarchy, blockers, related links, cross-project links, and cross-project
  ready views.
- BM25-ranked issue search, look-alike duplicate protection, create
  idempotency keys, and safe retry behavior for automation.
- Interactive `kata tui` with project switching, nested and flat issue lists,
  issue detail pages, filters, search, inline issue creation, editor-backed
  body/comment editing, mutations, help, split-pane layout, and realtime event
  refresh.
- Durable event polling and SSE streams, including `kata events --tail` for
  NDJSON consumers and reset handling after destructive history changes.
- Daemon hooks with TOML configuration, bounded queues, worker pools, timeouts,
  output capture, log rotation, pruning, and reload support.
- JSONL export/import for backup, migration, schema cutovers, and project
  restore workflows, plus importer support for Beads projects.
- Hub-and-spoke federation for shared kata projects, including enrollment,
  actor attribution, shared daemon catalogs, TUI daemon switching, and
  reversible spoke leave/rejoin.
- Remote-client mode with trusted private-network bearer auth, token-based
  identity, trusted-proxy actor headers, and explicit daemon selection.
- Hosted-mode daemon support for platforms that provide a `$PORT` contract,
  including Cloud Run, Render, Fly.io, Railway, App Engine, and similar hosts.
- Windows daemon support alongside Unix socket operation, with runtime addresses
  published as URLs for clients.
- OpenAPI support with `kata openapi`, a committed schema, generated client
  artifacts, and API schema version reporting in `/health`.
- Release tooling for annotated tags, tag verification, GitHub release
  artifacts, self-update discovery through `kata update --check`, and
  installable release packages.

**Improvements**

- CLI ergonomics now include `kata version`, `kata whoami`, `kata health`,
  project list/show/rename/merge/restore commands, daemon
  start/status/stop/log/reload commands, and `--comment` support on mutation
  commands.
- Agent-facing output gained a stable text format, better close-justification
  safeguards, claim-oriented ready filters, and `kata init --with-agents` for
  writing kata guidance into `AGENTS.md` or `CLAUDE.md`.
- TUI rendering gained hierarchy controls, labels in list/detail views,
  responsive narrow layouts, strict status filters, ANSI-safe text handling,
  no-color support, bracketed paste, opt-in mouse support, and cleaner
  reconnect behavior.
- Documentation now covers installation, quickstart workflows, agent usage,
  configuration, remote daemons, federation, hosted deployment, backup/restore,
  OpenAPI compatibility, SaaS issue tracker comparisons, design notes, and
  docs-site maintenance.
- Release builds now publish archives, checksums, Linux packages, Windows
  binaries, and metadata names that match the installer and updater discovery
  contract.

**Bug fixes**

- Fixed active daemon selection, stale daemon upgrades, remote-client
  resolution, named daemon auth isolation, and schema-skew quarantine recovery.
- Hardened credential routing so local daemon tokens, catalog daemon tokens,
  and explicit `--hub-token` paths do not cross unintended origins.
- Hardened JSONL import/export, destructive command confirmation, purge audit
  records, schema migration paths, and orphaned-state recovery.
- Improved SQLite contention handling with retries for transient write locks.
- Fixed percent-encoded git remote URL handling during init.
- Fixed optional OpenAPI client response modeling and generated-artifact drift
  checks.
- Relaxed close throttling so safeguards remain useful without blocking
  legitimate sibling issue closures.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for TUI
  navigation and presentation work, Beads import and priority/schema-cutover
  support, SQLite contention retries, generated OpenAPI client support, issue
  project moves, daemon lifecycle/git helper adoption, telemetry, and module
  path migration.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for agent guidance
  consolidation, hosted-mode `$PORT` binding and docs, trusted-proxy actor
  headers, active-daemon client handling, Unix runtime URL publication, and the
  storage abstraction/Postgres schema shell.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for `kata init
  --with-agents`, `kata openapi`, the committed OpenAPI schema, `/health`
  API schema version reporting, and API compatibility documentation.
- Thanks to [Andy Hadjigeorgiou](https://github.com/andyxhadji) for ready
  filters, claim workflows, and the cross-project `kata ready --all` view.
- Thanks to [Nat Torkington](https://github.com/njt) for Windows daemon support
  and percent-encoded git remote URL handling during init.
- Thanks to [Jesse Vincent](https://github.com/obra) for consolidating
  relationship editing into create/edit flags and adding `--comment` support to
  mutation commands.
- Thanks to [Chris K Wensel](https://github.com/cwensel) for the
  `KATA_HTTP_TIMEOUT` environment setting, project reset-counter support, and
  early opt-in remote-client mode.
- Thanks to [Hugh Brown](https://github.com/hughdbrown) for the per-actor
  activity digest over kata's event stream.

---

## Project History

### 2026-W25 (Jun 15 - Jun 21, 2026)

- Added versioned-release planning, changelog generation, annotated tag
  creation, tag verification, GitHub artifact workflow, self-update command
  design, and release documentation.
- Added cross-project links that survive issue moves.
- Added global daemon selection and fixed named-daemon authentication
  precedence across CLI, health checks, and TUI paths.
- Fixed federation schema-skew quarantine recovery and improved agent recovery
  when follow-up comments fail after mutations.
- Relaxed close throttling so bursty but legitimate sibling closes remain
  possible with proper evidence.

### 2026-W24 (Jun 8 - Jun 14, 2026)

- Added reversible federation leave/rejoin flows across CLI, daemon, and TUI.
- Added issue project move support and generated OpenAPI clients.
- Documented API compatibility and added `/health` API schema version reporting.
- Added generated API artifact drift checks before push.
- Fixed optional generated-client response objects and SQLite transient write
  contention handling.
- Improved `kata init --with-agents` handling for `CLAUDE.md`.

### 2026-W23 (Jun 1 - Jun 7, 2026)

- Added private-network remote-client mode, bearer-token safeguards, trusted
  proxy actor headers, simple token identity, and hosted daemon support.
- Added federation enrollment UX with actor-bound hub support.
- Added `kata init --with-agents`, flat TUI issue lists, OpenAPI schema
  generation, API docs, and docs screenshot hydration.
- Added docs infrastructure, SaaS tracker comparison material, Vercel deployment
  helpers, and curated design documentation.
- Added Windows daemon support, Unix runtime URL publication, daemon lifecycle
  adoption, telemetry, and storage abstraction groundwork.

### 2026-W22 (May 25 - May 31, 2026)

- Built the first hub-and-spoke federation workflows for enrollment,
  pull/push sync, quarantine, trust-boundary documentation, and local-first
  project adoption.
- Added shared daemon catalogs with TUI switching and trusted private-network
  bearer auth.
- Added cross-project ready queues, ready filters, claim workflows, and stable
  agent output formatting.
- Added project archive/restore behavior and safer list/import handling for
  larger project data.
- Hardened JSONL export/import, backup guidance, and WAL checkpoint
  documentation.
- Added hosted-mode support for `$PORT` platforms and consolidated agent
  guidance under `AGENTS.md`.

### 2026-W21 (May 18 - May 24, 2026)

- Switched the Go module path to `go.kenn.io/kata`.
- Continued TUI hierarchy, split-detail, and navigation polish.
- Prepared project restore, remote, hosted, and federation work that landed in
  the following week.

### 2026-W20 (May 11 - May 17, 2026)

- Added short issue refs derived from ULIDs in place of per-project numbers.
- Added close justification safeguards, anti-abuse guardrails, `--comment` on
  mutation commands, `kata version`, install docs, and backup/restore docs.
- Added daemon metadata, recurrence, auth, and issue move foundations.
- Fixed remote-client resolution and schema cutover behavior for pre-existing
  foreign-key orphans.

### 2026-W19 (May 4 - May 10, 2026)

- Added project views, cross-project issue APIs, project archival, early
  remote-client mode, and the first public PR contributions.
- Added TUI split-detail navigation fixes, child graph ordering, list header
  redesign, opt-in mouse support, marker-gutter polish, and unified detail
  scrolling.
- Added the Beads importer, first-class priority support, and schema cutover
  model.
- Added SQLite lock retries, daemon stale-socket cleanup, module version
  revision formatting, and percent-encoded git remote init fixes.
- Consolidated relationship editing into create/edit flags.

### 2026-W18 (Apr 27 - May 3, 2026)

- Bootstrapped kata's local-first architecture: project binding, SQLite
  storage, daemon API, runtime discovery, Cobra CLI root, and test tooling.
- Added issue/comment lifecycle storage, events, project initialization,
  health checks, lifecycle smoke tests, and daemon handler coverage.
- Added relationships, labels, ownership, ready queries, search, idempotency,
  soft delete, restore, purge, and purge auditing.
- Added polling and SSE events, `kata events --tail`, reset handling, and hook
  execution with queues, worker pools, captured output, rotation, pruning, and
  reload.
- Built the first full TUI, then expanded it with filters, search, editor
  integration, help, hierarchy rendering, responsive layouts, label chips,
  split-pane mode, and document-style detail pages.
- Added JSONL export/import, schema cutover support, stable UID references, and
  the federation foundation.
