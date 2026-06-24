---
title: Changelog
description: Release history for kata
---

All notable changes to kata, grouped by release. Versioned releases start with
0.5.0; earlier entries are a retroactive project history grouped by ISO week.

## 0.6.0
<small>2026-06-24</small>

kata 0.6.0 expands kata's release and sharing surface: GitHub Issues can now
feed a kata project, private-network daemon deployments have an explicit
tokenless write mode for trusted single-user networks, and Windows users have a
hosted release installer.

**New features**

- Added one-way GitHub issue sync with `kata sync github`, backed by
  daemon-owned bindings, cursors, import mappings, status, and a poller. The
  first provider imports GitHub issues and issue comments through the daemon's
  `gh api` environment, skips pull requests, prefixes imported titles by
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
