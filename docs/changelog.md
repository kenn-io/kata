---
title: Changelog
description: Release history for kata
last_edited: 2026-08-12
---

All notable changes to kata, grouped by release. Versioned releases start with
0.5.0; earlier entries are a retroactive project history grouped by ISO week.

## Unreleased

**New features**

- Added paired `kata schedule` / `kata deadline` commands and
  `kata.set_schedule` / `kata.set_deadline` MCP tools for first-class planning
  date updates.
- Expanded the native MCP server from the original project-bound issue tools
  to complete scoped issue, project, token, federation, synchronization,
  recurrence, import, digest, and live-event workflows.
- Scoped `kata mcp serve` to the current workspace project by default, with
  explicit one-project (`--project`/`--workspace`), fixed-allowlist
  (`--projects`, pinned by immutable UID and rename-safe), and daemon-wide
  (`--all-projects`) boundaries. Scoped graph, pagination, token, and
  federation operations cannot cross their startup boundary, and an allowlist
  keeps serving its remaining projects when one member is archived or merged
  away.
- Added 13 section loaders that progressively disclose the detailed typed MCP
  tools and publish tool-list change notifications.
- Added first-class MCP scheduling fields, force-create and force-claim, and
  generic metadata support for native markers such as `someday=true`.
- Added explicit host-local JSONL export and configured-target import tools
  behind `--storage-root` and startup target aliases. Storage operations stay
  anchored to the configured root, protect active SQLite databases and
  sidecars, require exact confirmation before replacing an artifact or target,
  and install restores race-safely with owner-only permissions.
- Gated daemon token administration behind the explicit `--enable-token-admin`
  capability in daemon-wide scope, kept federation topology changes
  (enrollment and spoke join) outside MCP entirely, restricted enabling
  issue synchronization — which selects the repository the daemon's GitHub
  credentials read — to the daemon-wide scope, and required that same scope
  for project administration: a scoped server can read its projects but
  cannot rename, merge, archive, restore, or purge them.
- Redacted cross-scope relationship identities, including parent, link,
  relationship-array, moved-project, close-evidence, close-throttle, and
  audit parent references, from scoped MCP issue, event, digest, edit-delta,
  and close-audit results, and rejected close-audit parent filters that
  cannot prove startup-scope membership.
- Compared PostgreSQL storage-import targets with the active daemon storage
  by persisted instance identity, so equivalent DSN spellings cannot bypass
  the textual overlap guard.

**Improvements**

- Gave deadlines the same date, local date-time, UTC-instant, and timezone
  projection rules as `scheduled_on`.
- Added schedule, deadline, and someday commands to generated agent guidance and
  public workflow documentation.
- Added `event_id` and the close-time frozen `parent_uid` to close-audit
  rows so audit results page with an immutable cursor and parent display
  refs carry provable provenance.

**Bug fixes**

- Restored the `clear_owner` and `clear_priority` recurrence template fields
  to the generated Go client; a stale template had silently dropped them from
  `pkg/client` since their introduction.

## 0.14.3
<small>2026-08-10</small>

**New features**

- Let Forge reuse Kata's issue details, including properties, checklists,
  links, and comments, through a network-free shared presentation package and
  bounded daemon reads.

**Improvements**

- Ensured release artifacts use the requested release tag throughout the
  automatic and manual publishing paths.

**Bug fixes**

- Restored login discovery for Web UI tabs opened directly, preserving the
  server-advertised authentication handoff.

## 0.14.2
<small>2026-08-10</small>

**New features**

- Added the official `kenn-io/tap/kata` Homebrew installation path for macOS
  and Linux.
- Added import support for SQLite-era Beads databases through the existing
  `kata import --source-format beads` workflow.
- Added a build-time distribution marker so Homebrew, `.deb`, `.rpm`, and
  third-party packages retain ownership of their installed binary while
  `kata update --check` remains available.
- Added a checksummed release source archive with production browser assets and
  vendored Go dependencies for reproducible, network-free Homebrew Core builds.

**Improvements**

- Upgraded `go.kenn.io/kit` from v0.14.0 to v0.19.1.
- Added prerelease-safe publishing so release candidates do not replace the
  latest stable GitHub release or update the stable Homebrew tap formula.
- Kept stable release notes based on the previous stable tag after publishing
  a release candidate from the same commit.

**Bug fixes**

- Protected edited TUI new-issue and new-child-issue drafts with discard
  confirmation when Esc is pressed, without prompting for untouched forms.
- Corrected `scheduled_on` timezone handling and civil-time semantics across
  readiness queries and browser snapshots.

## 0.14.1
<small>2026-08-08</small>

kata 0.14.1 adds a first-class browser workspace and native MCP integration,
expands cross-project and label-filtered discovery, and makes release,
federation, and daemon operations safer and more observable.

**New features**

- Added a daemon-served Web UI for managing projects, issue collections,
  fields, comments, checklists, relationships, recurrences, and multiple
  configured daemons.
- Added a native, project-bound stdio MCP server with 13 structured issue
  tools, protocol negotiation, fixed actor attribution, and bounded results.
- Added `kata list --all`, with status, priority, owner, label, exclusion, and
  metadata filters that compose across all non-archived projects.
- Added repeatable `--label` and `--no-label` filters to project-scoped
  `kata search`, applied before lexical limits and during vector-result
  hydration.
- Allowed `kata tui <issue-ref>` to resolve and open an issue's detail view
  directly.
- Exposed the standard Go profiling handlers under `/debug/pprof/` on the
  daemon's existing authenticated listener.

**Improvements**

- Centralized Claude Code and Codex hook configuration on kit's shared
  agent-hook manager while preserving the existing init flags, lifecycle
  matchers, attention behavior, unrelated configuration, and workspace
  symlink boundary.
- Added field-scoped Markdown rendering to `kata show --render` for issue
  descriptions and comments while preserving literal record structure and
  machine-readable output.
- Made `ready --all` and `next --all` compose with owner and label filters.
- Excluded issues parked with `someday=true` or a future `scheduled_on` date
  from scoped, global, and browser ready queues.
- Limited workspace binding discovery to the current Git root, preventing an
  ancestor repository's `.kata.toml` from capturing a nested repository.
- Avoided federation-wide link reconciliation for batches that cannot affect
  links and gave synchronization requests their own bounded timeout.
- Kept the release installers compatible with releases that predate embedded
  Web UI validation while requiring current releases to validate their assets.
- Added daemon API-version preflights before filtered global list/ready and
  filtered search operations, so older daemons fail with an upgrade message
  instead of silently ignoring filters.

**Bug fixes**

- Kept GoReleaser validation from dirtying the release checkout before archive
  publication, allowing the release workflow to validate and publish the
  embedded Web UI artifacts successfully.

**Acknowledgements**

- Thanks to [Rusty Shackleford](https://github.com/salmonumbrella) for the
  native MCP server, cross-project list filters and version checks, scoped
  search label filters, parked-ready semantics, and direct TUI issue opening.
- Thanks to [Wes McKinney](https://github.com/wesm) for the Web UI, bounded
  federation work, Git-root workspace discovery, legacy installer support,
  and clean GoReleaser validation.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for Markdown rendering
  in `kata show`.
- Thanks to [Joi Ito](https://github.com/Joi) for composing `ready --all` and
  `next --all` with scoped filters.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the
  standard daemon profiling endpoints.

## 0.13.0
<small>2026-07-31</small>

kata 0.13.0 makes federation endpoint changes safer, automates enrollment,
and improves agent and embedded-service integrations.

**New features**

- Added explicit, retry-safe `kata federation rebind` endpoint migration for
  moving one spoke or every local spoke to a named HTTPS hub catalog entry.
  The daemon validates the unchanged hub project identity with the existing
  enrollment and drains old-endpoint sync work before preserving tokens,
  capabilities, actors, and sync cursors across the URL/security-only update.
- Added startup-configured spoke-to-hub project mappings that create or adopt
  local projects, enroll them idempotently, retry hub failures without
  affecting daemon readiness, and expose sanitized aggregate progress in
  `/health`. Federation teardown remains an explicit `kata federation leave`.
- Added project-scoped federation enrollment creation, history, and revocation
  methods to the mountable Go service for hosts that use the restricted
  embedding profile.
- Added `kata init --with-codex-hooks` to install the `work.attention` harness
  into a Codex CLI workspace's `.codex/hooks.json`. Only the `SessionStart`
  half is wired, since Codex has no stable session-end hook event yet; cover
  the end half with a launcher wrapper until upstream ships one.

**Improvements**

- Kept remote TUI project resolution path-free by using only portable project
  names and Git identities for configured remote daemons.
- Authenticated federation ingest before body decoding and added bounded host
  admission for large uploads, returning a retryable `429` response when an
  embedding host is saturated.
- Added federation credential host validation and optional host-side access
  revalidation so credentials remain bound to their configured origin and
  embedded hosts can deny revoked outside authority without replacing Kata
  authentication.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for federation endpoint
  rebinding, configuration-driven enrollment, embedded federation lifecycle
  controls, path-free remote TUI resolution, safer federation ingest, and
  credential host validation.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for the Codex CLI
  attention hooks.

## 0.12.1
<small>2026-07-21</small>

kata 0.12.1 hardens daemon credential routing, embedded-service boundaries, and
repeatable query filters.

**Improvements**

- Added restricted embedding policies and transaction fences so host-owned
  authorization is enforced at the same boundary as Kata writes and worker
  operations.
- Preserved repeated values in query filters, including label, exclusion,
  metadata, actor, and digest filters.

**Bug fixes**

- Refused Git-tracked `.kata.local.toml` files so a committed daemon redirect
  cannot route a global bearer token to an untrusted origin.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for the tracked-local
  configuration guard that prevents daemon credential misrouting.
- Thanks to [Wes McKinney](https://github.com/wesm) for restricted embedding
  policies, transaction fences, and repeated query-filter handling.

## 0.12.0
<small>2026-07-20</small>

kata 0.12.0 gives embedded integrations host-controlled access policies and
project lifecycle APIs, and makes TUI input safer and easier to navigate.

**New features**

- Added host-controlled authentication and per-operation authorization for
  mounted Go services through `Config.Access`. Embedding hosts supply the
  authenticated principal; kata exposes the matched operation and project
  scope to their controller, records the host-supplied actor, hides denied
  resources, and revalidates access during long-lived event streams.
- Added retry-safe `Service.EnsureProject` and `Service.ArchiveProject` methods
  so embedding hosts can provision exact project UID/name bindings and archive
  projects without deleting their stable identity, task history, or events.

**Improvements**

- Made Ctrl-O the canonical TUI save/apply chord while retaining Ctrl-S as a
  compatibility alias for existing users.
- Allowed Up and Down to navigate live search results before committing the
  filter, with reversible query/results focus and an explicit keep-filter exit.
- Protected non-empty TUI comment drafts with discard confirmation when Esc is
  pressed; canceling preserves the full in-memory form state, and the modal
  footer advertises the active discard and keep-editing actions.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for host-controlled
  embedded access, project lifecycle APIs, and safer TUI input and navigation.

## 0.11.1
<small>2026-07-19</small>

kata 0.11.1 makes pgvector optional for PostgreSQL deployments that do not use
semantic search.

**Improvements**

- Allowed core PostgreSQL task, federation, and lexical-search storage to start
  without pgvector. Kata creates vector tables only when pgvector 0.7 or later
  is already installed in `public`; configuring semantic search without it
  reports the feature as unavailable without affecting core storage.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for optional pgvector
  support in PostgreSQL deployments.

## 0.11.0
<small>2026-07-18</small>

kata 0.11.0 completes PostgreSQL support across kata's storage and sharing
workflows and exposes the application as a listener-free Go service. It also
strengthens machine-readable identity and semantic-search input validation.

**New features**

- Added complete PostgreSQL support for the storage contract, daemon operation,
  federation, claims, external and JSONL import, export and atomic snapshot
  restore, lexical search, and pgvector-backed semantic search. Configure it
  with `KATA_DSN`, `[storage].dsn`, or a `postgres://` / `postgresql://` URL.
- Added `kata storage postgres migrate` and `status`, validation-only runtime
  configuration, server-identity-verified TLS requirements, and separate
  schema-owner and serving roles for production PostgreSQL deployments.
- Exposed the module root at `go.kenn.io/kata` as a mountable Go service.
  Applications construct a service with `kata.New`, mount its HTTP `Handler`,
  run federation, GitHub sync, and timed-claim workers with `Run`, and release
  owned storage and workers with `Close`; kata does not take over the listener,
  signals, or host process lifecycle.

**Improvements**

- Added the stable canonical identity `"name": "kata"` to `kata version
  --json`, alongside the existing version and build fields, so automation can
  identify the binary without parsing human-readable output or contacting a
  daemon.
- Rejected embedding responses with null components or zero-norm vectors before
  they can be persisted or used for a query. Background fills remain pending
  and retry after an invalid provider response instead of marking unusable
  vectors complete.

**Bug fixes**

- Honored `KATA_HTTP_TIMEOUT` for probes of explicitly configured remote
  servers, while keeping the separate one-second local runtime-discovery probe.
  Slow healthy WAN and reverse-proxied daemons now get the same request budget
  as the command that follows the probe.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for complete PostgreSQL
  support, the mountable Go service, and configured-remote timeout handling.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for stable
  JSON version identity and invalid embedding-vector rejection.

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
