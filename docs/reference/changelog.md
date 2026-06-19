# Changelog

kata starts versioned releases with `v0.5.0`. Earlier work happened as rapid
pre-release development, so this page includes a retroactive project history
grouped by ISO week.

## Releases

### v0.5.0

The first versioned kata release. Release notes for this version will be
generated from the annotated tag when the release is cut.

Highlights:

- Repeatable release scripts for changelog generation, annotated tag creation,
  and push.
- GitHub release artifacts named for kit self-update discovery.
- `kata update --check` and `kata update` for release discovery and install.
- TUI title-bar coverage for stamped release versions.

## Project History

### 2026-W25 (Jun 15 - Jun 21, 2026)

- Added versioned-release planning, release scripts, GitHub artifact workflow,
  self-update command design, and release documentation.
- Expanded hosted-mode, remote-daemon, and federation operations docs.
- Hardened cross-project links, move-safe relationships, generated clients, and
  schema compatibility checks.

### 2026-W24 (Jun 8 - Jun 14, 2026)

- Added reversible federation leave/rejoin flows across CLI, daemon, and TUI.
- Documented API compatibility, health versioning, and hosted deployment
  expectations.
- Improved init agent-file handling and generated API drift checks.

### 2026-W23 (Jun 1 - Jun 7, 2026)

- Added private-network remote-client mode, bearer-token safeguards, and hosted
  daemon support.
- Expanded federation catalog, token, adoption, and sync recovery workflows.
- Improved project views, issue movement, and multi-project TUI navigation.

### 2026-W22 (May 25 - May 31, 2026)

- Built the first federation workflows for hub-and-spoke replication, enrollment,
  pull/push sync, quarantine, and trust-boundary documentation.
- Added project archive/restore behavior and project-scoped operations.
- Strengthened daemon credential routing and recovery paths.

### 2026-W21 (May 18 - May 24, 2026)

- Introduced export/import, JSONL backup and restore, OpenAPI generation, and
  published client artifacts.
- Expanded documentation structure with guide, reference, workflow, operations,
  and design sections.
- Continued TUI redesign work for forms, split view, rendering polish, and
  keyboard ergonomics.

### 2026-W20 (May 11 - May 17, 2026)

- Built the main TUI: list and detail views, filtering, inline create/edit
  flows, help overlay, SSE refresh, and golden snapshot coverage.
- Added hook execution, hook logs, rotation, pruning, daemon reload, and
  end-to-end hook verification.
- Added event polling and live tailing with reset handling.

### 2026-W19 (May 4 - May 10, 2026)

- Added relationships, labels, ownership, ready queries, search, idempotency,
  soft delete, restore, purge, and event auditing.
- Expanded the CLI around create/show/list/edit/comment/close/reopen and the
  destructive verbs.
- Added full-lifecycle smoke tests and daemon handler coverage.

### 2026-W18 (Apr 27 - May 3, 2026)

- Bootstrapped kata's local-first architecture: project binding, SQLite storage,
  daemon API, runtime discovery, and the Cobra CLI root.
- Added core issue/comment lifecycle storage, event emission, health checks, and
  project initialization.
- Established the first design specs, implementation plans, test environment,
  and build tooling.
