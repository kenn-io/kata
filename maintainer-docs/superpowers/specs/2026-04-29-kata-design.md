# kata — Lightweight Issue Tracker for Agents

**Status:** Design (v1) — partially superseded
**Date:** 2026-04-29 (revised: project model, .kata.toml binding, KATA_HOME, DATETIME columns)
**Topic:** kata — a local SQLite + daemon + TUI issue tracker, agent-first, modeled on the roborev modality.

> **Superseded sections:** the relationship-editing CLI (§3.4 row for `link`/`unlink`/`parent`/`block`/`relate` and the §6 "Relationships" command-table block) and the per-link `issue.linked` / `issue.unlinked` event model for PATCH-driven mutations have been replaced. The shipped surface is `kata create` / `kata edit` with `--parent` / `--blocks` / `--blocked-by` / `--related` (and matched `--remove-*` flags), atomic PATCH semantics, and a single aggregated `issue.links_changed` event per mutation. The two `kata edit` line and the entire `Relationships` block below are retained for historical context only — read the current help text (`kata edit --help`) for shipped behavior.

## 1. Overview

kata replaces ad-hoc use of GitHub Issues for agent task-tracking. It is a single-binary local tool with a long-lived daemon, a SQLite database, a CLI, and a Bubble Tea TUI. Agents are the primary writers; humans observe and steer through the TUI.

The shape is borrowed deliberately from roborev: pure-Go SQLite via `modernc.org/sqlite`, a Huma-based HTTP API on a Unix socket (TCP loopback fallback on Windows), per-PID runtime files, durable SSE event stream, and directory-style installable agent skills. Where roborev runs review/fix workloads, kata runs only issue CRUD plus an event broadcaster and a small bounded hook runner — there is no agentic review worker pool and no daemon-driven code execution beyond the user's own configured hook scripts.

A **project** is the issue namespace. One project may aggregate one or more git repositories (or a non-git workspace) — useful for monorepos and for two GitHub repositories that ship together. Issues, numbering, links, and labels all live within a project. The mapping from a workspace directory to its project is established explicitly via a committed `.kata.toml` file at the repository root.

The design optimizes for three things:

1. **Agent ergonomics.** Stable JSON, stable exit codes, search-before-create, idempotency keys with fingerprints, structured error envelopes, no implicit `$EDITOR` invocation in machine paths. **Explicit project binding** — agents never auto-create projects from a random cwd; the workspace declares its project via committed config.
2. **Auditability.** Every state change appends to an immutable `events` table with the actor recorded. Comments are append-only. Deletion has a soft tier (`kata delete --force`) and a hard tier (`kata purge --force --confirm`) gated by interactive prompt or exact-string flag. The hard tier writes an out-of-cascade `purge_log` row.
3. **A small, sharp surface.** Issue lifecycle, three relationship types (`parent`, `blocks`, `related`), labels, owners, comments. No `in_progress` status, no severities, no priorities, no attachments, no threaded replies, no markdown rendering.

## 2. Architecture

```
CLI (kata) ──HTTP/JSON──> Daemon ──> SQLite ($KATA_HOME/kata.db, WAL, FK ON)
                            │
                            ├─> SSE event broadcaster (durable, resumable)
                            └─> Bounded hook runner (post-commit, no shell)

TUI (kata tui) ──HTTP + SSE──> Daemon
```

### 2.1 Stack

- Go, single binary. Pure-Go SQLite (`modernc.org/sqlite`). WAL mode.
- Huma for HTTP API + OpenAPI generation.
- Cobra for CLI. Bubble Tea + Lipgloss for TUI.
- testify for tests. Table-driven, `t.TempDir()`, `-shuffle=on`. No CGO.
- All timestamps UTC, RFC3339 at API boundaries, RFC3339 with milliseconds at SQLite boundary. Schema timestamp columns are typed `DATETIME` so the SQLite driver round-trips them as `time.Time`.

### 2.2 Daemon transport

The daemon listens on one of:

- **Unix socket (default on Unix)**: parent dir `0700`, socket `0600`.
- **TCP loopback (default on Windows; opt-in elsewhere)**: address validated to be loopback (`127.0.0.1`, `::1`, `localhost`); port auto-increments from `7474` if busy.

`DaemonEndpoint` abstraction handles both transports (matches roborev's `internal/daemon/endpoint.go`).

### 2.3 Per-PID runtime files; DB-namespaced

```
$KATA_HOME/runtime/<dbhash>/
  daemon.<pid>.json     # 0644, atomic write-and-rename
  daemon.log            # rotated 10MB × 5
```

`<dbhash>` = first 12 hex chars of `sha256(absolute(effective_db_path))`. Two `KATA_DB` instances never collide on runtime files, sockets, or logs.

The socket path is **recorded inside** the runtime file. Socket itself lives in `$XDG_RUNTIME_DIR/kata/<dbhash>/daemon.sock` (ephemeral runtime dir is fine; runtime file in data dir is what makes it discoverable).

Clients discover the daemon by:

1. Compute `<dbhash>` from effective DB path.
2. Scan `$KATA_HOME/runtime/<dbhash>/daemon.*.json`.
3. For each, probe `GET /api/v1/ping`. Live = use it; dead = clean up the file.
4. If none live, auto-start daemon.

Cleanup of stale files via `ListAllRuntimes` + `/ping` liveness probe.

### 2.4 Project resolution & aliases

A **project** is the issue namespace. `projects.next_issue_number` owns the issue numbering for one or more workspaces.

A **project alias** maps a workspace location to a project. One project may have many aliases. `project_aliases.alias_identity` is `UNIQUE` (an alias points to exactly one project), but multiple aliases may point to the same project_id.

#### Path discovery

Every CLI invocation passes a **start path** (the value of `--workspace <path>` if given, else the process `cwd`). The daemon resolves a workspace from the start path by walking upward:

1. Find the first ancestor directory containing `.kata.toml` (inclusive of the start path itself). Call this `W` (workspace root).
2. Find the first ancestor directory containing a `.git` entry. Call this `G` (git root). `G` and `W` may differ when `.kata.toml` lives in a subdirectory of a larger git repo, or coincide when `.kata.toml` is at the repo root.

The CLI never walks up itself — it sends the start path as-is. The daemon owns discovery so all clients (CLI, TUI, future) see identical resolution.

#### Alias identity

`alias_identity` is computed from `G` (the **git root**, not from `W`):

1. If `G` exists and a git remote is present:
   - Use `origin` URL if set, else the first remote listed by `git remote`.
   - Strip embedded credentials (`https://user:pass@host/...` → `https://host/...`).
   - Normalize SSH↔HTTPS variants (`git@github.com:foo/bar.git` → `github.com/foo/bar`).
   - `alias_kind = "git"`.
2. If `G` exists with no remotes: `alias_identity = "local://<absolute_path_of_G>"`, `alias_kind = "local"`.
3. If `G` does not exist but `W` does (workspace declared inside a non-git directory): `alias_identity = "local://<absolute_path_of_W>"`, `alias_kind = "local"`.
4. If neither `G` nor `W` exists, no alias is computed (resolution fails).

`alias_identity` is the stable handle. Cloning a git repo to a new path keeps the same `alias_identity` (case 1) since it derives from the remote URL.

#### Project resolution (used by every command except `kata init`)

Given a start path and no explicit project name:

1. **Alias lookup first.** If `G` exists, compute `alias_identity` and look up `project_aliases`. On match, resolve to that project; update `last_seen_at`/`root_path`. If a stale `.kata.toml` name differs, rewrite it to the resolved project name.
2. **Else, `.kata.toml` name.** If `W` exists and `<W>/.kata.toml` declares `[project] name = "..."`, look up `projects WHERE name = <toml.project.name>`. If no row exists, fail with `project_not_initialized` (hint: run `kata init --project <name>` in this workspace).
3. **Else, fail.** Return `project_not_initialized` with hint `run "kata init" in this workspace`.

Outside any git repo and without `.kata.toml`, every command except `kata init --project <X>` fails. kata never silently namespaces issues to "a random cwd."

An explicit CLI `--project <name>` bypasses workspace discovery and resolves by exact project name. `.kata.toml` parsing and `alias_identity` derivation are performed daemon-side from the `start_path` the CLI sends when no explicit project is provided.

#### `kata init` semantics

`kata init` is the **only** command that creates a `projects` row. Resolution flow inside `kata init`:

1. Walk upward from the start path to find `W` (first ancestor with `.kata.toml`) and `G` (first ancestor with `.git`).
2. **Fresh-clone flow — existing `.kata.toml` and no flags.** If `W` is found and `<W>/.kata.toml` contains a valid binding:
   - Look up or create `projects WHERE name = <toml.project.name>`. The project row is materialized here, on demand from the committed config — this is the only path that does so.
   - Compute the alias from `G` (or `local://<abs(W)>` when `G` is absent). If the alias is unattached, attach it. If attached to a *different* project, fail with `project_alias_conflict` unless `--reassign` was passed.
   - If `--project <name>` was also passed and disagrees with `<toml.project.name>`, fail with `project_binding_conflict` unless `--replace` was passed.
   - The `.kata.toml` is left as-is when content matches (idempotent); written when `--replace` overrides the name.
3. **No `.kata.toml`, with `--project <name>`.** Look up or create the project; compute and attach the alias (same conflict rules); write `<G>/.kata.toml` (or `<start_path>/.kata.toml` when `G` is absent).
4. **No `.kata.toml`, no `--project` flag.**
   - If `G` exists with a remote: derive the project name from the normalized origin URL's last segment. Proceed as step 3.
   - If `G` exists with no remote: derive the project name from the directory basename. Proceed as step 3.
   - If `G` does not exist: fail with usage error (`cd` into a workspace, or pass `--project`).

`--replace` allows overwriting `.kata.toml` whose name disagrees with `--project`. `--reassign` allows moving an existing alias to this project. Neither is needed for the common fresh-clone case.

Two repos sharing a project: each commits `.kata.toml` with `name = "system"`. Running `kata init` in each (no flags) succeeds, attaching both aliases to the same project. Issue numbering is shared.

**Monorepo with multiple projects per git repo.** Because `alias_identity` is `UNIQUE` per git origin URL, one git repo can attach to only one project in v1. Subdirectories with their own `.kata.toml` declaring a different project fail with `project_alias_conflict` until the operator runs `kata init --reassign` (which moves the single available alias). Operators who need genuine multi-project monorepos should accept one project per git repo for v1; v2 may add path-scoped aliases.

### 2.5 Read/write split

All v1 reads go through the daemon. No direct SQLite access from the CLI in v1.

A future `OpenReadOnly` (no migrations, no PRAGMA mutation, schema-version check) can land later for hot paths if measured. Don't pay that complexity tax until measured.

### 2.6 Stable IDs and SSE durability

- Projects and issues have immutable ULID strings in `projects.uid` and `issues.uid`. `uid` is the authoritative external identity; `#N` / `number` is the human display label scoped to one project.
- Links store both endpoint UIDs (`from_issue_uid`, `to_issue_uid`) and endpoint integer FKs (`from_issue_id`, `to_issue_id`). The integer FKs remain the hot-path join cache; triggers reject drift between the UID columns and the FK targets.
- Event envelopes have monotonic `event_id` (= `events.id`), `actor`, `project_id`, `project_uid`, `project_name` (snapshot), `issue_id`, `issue_uid`, `issue_number`, `related_issue_id` (nullable), `related_issue_uid` (nullable), `type`, `payload`, `created_at`.
- Persisted rows live in the `events` table (which is also the audit trail). `project_uid` is derived from `projects.uid`; issue UIDs are stored on the event row so issue identity survives issue-row deletion until purge removes the events.
- Daemon broadcasts only **after DB commit**, with the row's `event_id` as the SSE `id:` field.
- **Purge reserves a synthetic SSE cursor** strictly greater than the current max `events.id`. Concretely: in the same transaction that purges an issue, if any events were deleted, the daemon advances `sqlite_sequence.seq` for `events` by one (without inserting a row) and stores that reserved value as `purge_log.purge_reset_after_event_id`. Future real `events.id` values continue from `reserved + 1`, so the synthetic cursor is unique and unattainable by any real event.
- On reconnect with `Last-Event-ID` (or `?after_id=N`; both → 400), daemon computes `MAX(purge_reset_after_event_id) FROM purge_log WHERE purge_reset_after_event_id > <cursor>`. The per-project stream (`?project_id=N`) adds `AND project_id = ?` so a purge in some other project can't invalidate this client's cursor; the cross-project stream omits the predicate. If the result is non-null, the client's cursor is invalidated. Because every reserved cursor exceeds every event id that existed at the corresponding purge time, even a client at max-at-purge will be reset (strict `>` is correct against a strictly-greater reserved value; no off-by-one miss, no need for `>=`).
- If invalidated, daemon sends a single `sync.reset_required` synthetic event with `id:` = the **MAX** of all matching `purge_reset_after_event_id`s, then closes the stream. Using the max ensures one reset moves the client past every accumulated purge gap; the client adopts that id as its new cursor and refetches state.

### 2.7 Hooks (preview, full design §8)

- After-DB-commit, async, bounded concurrency (default pool=4, queue=1000), per-hook timeout (default 30s, max 5m).
- `exec.Command(cmd, args...)`. **No shell, no env-var expansion of args.** Data flows via JSON on stdin and a small set of `KATA_*` env scalars.
- Configured globally only in v1. Workspace-local hook config is out of scope for v1 (see §10).

### 2.8 Auditability summary

- `events` table is append-only and authoritative for state changes.
- `comments` table is append-only — no edit, no delete (purge only).
- Issues are mutable in their current-state row; the events log captures every mutation with field diffs in `payload`.
- Soft-delete sets `deleted_at`; reversible via `kata restore`.
- Purge requires `kata purge <id> --force --confirm "PURGE #N"`, writes a `purge_log` row that survives the cascade and includes `issue_uid` / `project_uid`, then physically removes `comments`/`links`/`labels`/`events` for the issue and the issue itself.

### 2.9 Browser CSRF defense

The HTTP server rejects any non-empty `Origin` header (including `null`), requires `Content-Type: application/json` on mutations, and emits no CORS headers. CLI/TUI never set `Origin` so they're unaffected; this prevents drive-by browser exploits against the loopback socket.

## 3. Data Model

The schema baseline is `internal/db/migrations/0001_init.sql`. Normal version upgrades are performed by JSONL export/import cutovers instead of in-place table rebuild migrations: export the source schema exactly as stored, import into a fresh database at the binary's current schema, apply importer fill rules for older `export_version`s, validate, then atomically swap database files. The migration runner records the binary's `currentSchemaVersion` after init; `0001_init.sql` does not seed `meta.schema_version`.

Beyond `schema_version`, the `meta` table also carries `instance_uid` — a single ULID identifying this kata installation, written by `db.Open` at first init and never changed afterwards (see federation-foundation spec §1.1). Every event and purge_log row carries `uid` + `origin_instance_uid` so the data is sync-ready: the daemon stamps `origin_instance_uid` from the local `meta.instance_uid` at insert time, and v2→v3 cutover backfills the same value onto historical rows.

### 3.1 DB open path

Every connection runs:

```sql
PRAGMA foreign_keys  = ON;       -- enforce FKs (default OFF; per-connection)
PRAGMA journal_mode  = WAL;
PRAGMA synchronous   = NORMAL;
PRAGMA busy_timeout  = 5000;
```

`foreign_keys=ON` is part of the data model contract.

Timestamp columns are typed `DATETIME` (not `TEXT`) so `modernc.org/sqlite v1.49.x` auto-parses them into `time.Time` on scan. Storage is still TEXT in RFC3339-with-millis form (`%Y-%m-%dT%H:%M:%fZ`); the column-type declaration is the signal the driver uses to drive the conversion.

### 3.2 Schema (0001_init.sql)

```sql
CREATE TABLE projects (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  uid               TEXT NOT NULL UNIQUE,
  name              TEXT NOT NULL UNIQUE,
  created_at        DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  next_issue_number INTEGER NOT NULL DEFAULT 1,
  CHECK (length(uid) = 26),
  CHECK (length(trim(name)) > 0)
);

CREATE TABLE project_aliases (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id      INTEGER NOT NULL REFERENCES projects(id),
  alias_identity  TEXT UNIQUE NOT NULL,    -- normalized git remote, or 'local://<abs path>'
  alias_kind      TEXT NOT NULL CHECK(alias_kind IN ('git','local')),
  root_path       TEXT NOT NULL,           -- last seen absolute workspace root for this alias
  created_at      DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  last_seen_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  CHECK (length(trim(alias_identity)) > 0),
  CHECK (length(trim(root_path)) > 0)
);
CREATE INDEX idx_project_aliases_project ON project_aliases(project_id);

CREATE TABLE issues (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  uid           TEXT NOT NULL UNIQUE,
  project_id    INTEGER NOT NULL REFERENCES projects(id),
  number        INTEGER NOT NULL,
  title         TEXT NOT NULL,
  body          TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL CHECK(status IN ('open','closed')) DEFAULT 'open',
  closed_reason TEXT CHECK(closed_reason IN ('done','wontfix','duplicate')),
  owner         TEXT,
  author        TEXT NOT NULL,
  created_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  closed_at     DATETIME,
  deleted_at    DATETIME,
  UNIQUE(project_id, number),
  CHECK (length(uid) = 26),
  CHECK (length(trim(title))  > 0),
  CHECK (length(trim(author)) > 0),
  CHECK (status = 'closed' OR (closed_at IS NULL AND closed_reason IS NULL))
);
CREATE INDEX idx_issues_project_status_updated
  ON issues(project_id, status, updated_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_issues_project_updated
  ON issues(project_id, updated_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_issues_owner
  ON issues(owner) WHERE owner IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE comments (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_id   INTEGER NOT NULL REFERENCES issues(id),
  author     TEXT NOT NULL,
  body       TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  CHECK (length(trim(author)) > 0),
  CHECK (length(trim(body))   > 0)
);
CREATE INDEX idx_comments_issue ON comments(issue_id, created_at);

CREATE TABLE links (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id    INTEGER NOT NULL REFERENCES projects(id),
  from_issue_id INTEGER NOT NULL REFERENCES issues(id),
  to_issue_id   INTEGER NOT NULL REFERENCES issues(id),
  from_issue_uid TEXT NOT NULL,
  to_issue_uid   TEXT NOT NULL,
  type          TEXT NOT NULL CHECK(type IN ('parent','blocks','related')),
  author        TEXT NOT NULL,
  created_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  UNIQUE(from_issue_id, to_issue_id, type),
  CHECK (from_issue_id <> to_issue_id),
  CHECK (length(from_issue_uid) = 26),
  CHECK (length(to_issue_uid) = 26),
  CHECK (length(trim(author)) > 0),
  CHECK (type <> 'related' OR from_issue_id < to_issue_id)
);
CREATE UNIQUE INDEX uniq_one_parent_per_child
  ON links(from_issue_id) WHERE type = 'parent';
CREATE INDEX idx_links_from    ON links(from_issue_id, type);
CREATE INDEX idx_links_to      ON links(to_issue_id, type);
CREATE INDEX idx_links_project ON links(project_id);
CREATE INDEX idx_links_from_uid ON links(from_issue_uid);
CREATE INDEX idx_links_to_uid   ON links(to_issue_uid);

-- Enforce same-project: both endpoints must belong to links.project_id.
CREATE TRIGGER trg_links_same_project_insert
BEFORE INSERT ON links
FOR EACH ROW BEGIN
  SELECT RAISE(ABORT, 'cross-project links are not allowed')
  WHERE (SELECT project_id FROM issues WHERE id = NEW.from_issue_id) <> NEW.project_id
     OR (SELECT project_id FROM issues WHERE id = NEW.to_issue_id)   <> NEW.project_id;
END;
CREATE TRIGGER trg_links_same_project_update
BEFORE UPDATE ON links
FOR EACH ROW BEGIN
  SELECT RAISE(ABORT, 'cross-project links are not allowed')
  WHERE (SELECT project_id FROM issues WHERE id = NEW.from_issue_id) <> NEW.project_id
     OR (SELECT project_id FROM issues WHERE id = NEW.to_issue_id)   <> NEW.project_id;
END;

CREATE TRIGGER trg_links_uid_consistency_insert
BEFORE INSERT ON links
FOR EACH ROW BEGIN
  SELECT RAISE(ABORT, 'from_issue_uid does not match from_issue_id')
  WHERE NEW.from_issue_uid <> (SELECT uid FROM issues WHERE id = NEW.from_issue_id);
  SELECT RAISE(ABORT, 'to_issue_uid does not match to_issue_id')
  WHERE NEW.to_issue_uid <> (SELECT uid FROM issues WHERE id = NEW.to_issue_id);
END;
CREATE TRIGGER trg_links_uid_consistency_update
BEFORE UPDATE ON links
FOR EACH ROW BEGIN
  SELECT RAISE(ABORT, 'from_issue_uid does not match from_issue_id')
  WHERE NEW.from_issue_uid <> (SELECT uid FROM issues WHERE id = NEW.from_issue_id);
  SELECT RAISE(ABORT, 'to_issue_uid does not match to_issue_id')
  WHERE NEW.to_issue_uid <> (SELECT uid FROM issues WHERE id = NEW.to_issue_id);
END;

CREATE TRIGGER trg_projects_uid_immutable
BEFORE UPDATE OF uid ON projects
FOR EACH ROW BEGIN
  SELECT RAISE(ABORT, 'projects.uid is immutable')
  WHERE NEW.uid <> OLD.uid;
END;
CREATE TRIGGER trg_issues_uid_immutable
BEFORE UPDATE OF uid ON issues
FOR EACH ROW BEGIN
  SELECT RAISE(ABORT, 'issues.uid is immutable')
  WHERE NEW.uid <> OLD.uid;
END;

CREATE TABLE issue_labels (
  issue_id   INTEGER NOT NULL REFERENCES issues(id),
  label      TEXT NOT NULL,
  author     TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY(issue_id, label),
  CHECK (length(label) BETWEEN 1 AND 64),
  CHECK (label NOT GLOB '*[^a-z0-9._:-]*'),
  CHECK (length(trim(author)) > 0)
);
CREATE INDEX idx_issue_labels_label ON issue_labels(label);

CREATE TABLE events (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  uid                 TEXT NOT NULL UNIQUE,
  origin_instance_uid TEXT NOT NULL,
  project_id          INTEGER NOT NULL REFERENCES projects(id),
  project_name    TEXT NOT NULL,
  issue_id            INTEGER REFERENCES issues(id),
  issue_uid           TEXT,
  issue_number        INTEGER,
  related_issue_id    INTEGER REFERENCES issues(id),
  related_issue_uid   TEXT,
  type                TEXT NOT NULL,
  actor               TEXT NOT NULL,
  payload             TEXT NOT NULL DEFAULT '{}',
  created_at          DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  CHECK (length(trim(actor)) > 0),
  CHECK (json_valid(payload)),
  CHECK (length(uid) = 26),
  CHECK (length(origin_instance_uid) = 26)
);
CREATE INDEX idx_events_project ON events(project_id, id);
CREATE INDEX idx_events_issue   ON events(issue_id, id) WHERE issue_id IS NOT NULL;
CREATE INDEX idx_events_related ON events(related_issue_id, id) WHERE related_issue_id IS NOT NULL;
CREATE INDEX idx_events_issue_uid ON events(issue_uid) WHERE issue_uid IS NOT NULL;
CREATE INDEX idx_events_related_issue_uid ON events(related_issue_uid) WHERE related_issue_uid IS NOT NULL;
CREATE INDEX idx_events_origin_instance ON events(origin_instance_uid);
CREATE INDEX idx_events_idempotency
  ON events(project_id, json_extract(payload, '$.idempotency_key'), created_at)
  WHERE type = 'issue.created' AND json_extract(payload, '$.idempotency_key') IS NOT NULL;

CREATE TABLE purge_log (
  id                          INTEGER PRIMARY KEY AUTOINCREMENT,
  uid                         TEXT NOT NULL UNIQUE,
  origin_instance_uid         TEXT NOT NULL,
  project_id                  INTEGER NOT NULL,   -- snapshot; no FK so audit survives any future project cleanup
  purged_issue_id             INTEGER NOT NULL,   -- the deleted issues.id; no FK (the row is gone)
  issue_uid                   TEXT,
  project_uid                 TEXT,
  project_name            TEXT NOT NULL,      -- snapshot of projects.name at purge time
  issue_number                INTEGER NOT NULL,
  issue_title                 TEXT NOT NULL,
  issue_author                TEXT NOT NULL,
  comment_count               INTEGER NOT NULL,
  link_count                  INTEGER NOT NULL,
  label_count                 INTEGER NOT NULL,
  event_count                 INTEGER NOT NULL,
  events_deleted_min_id       INTEGER,            -- audit (min events.id deleted; NULL if none)
  events_deleted_max_id       INTEGER,            -- audit (max events.id deleted; NULL if none)
  purge_reset_after_event_id  INTEGER,            -- SSE reset cursor; subscribers with cursor < this must reset
  actor                       TEXT NOT NULL,
  reason                      TEXT,
  purged_at                   DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  CHECK (length(trim(actor)) > 0),
  CHECK (length(uid) = 26),
  CHECK (length(origin_instance_uid) = 26)
);
CREATE INDEX idx_purge_log_reset
  ON purge_log(purge_reset_after_event_id) WHERE purge_reset_after_event_id IS NOT NULL;
CREATE INDEX idx_purge_log_project_reset
  ON purge_log(project_id, purge_reset_after_event_id) WHERE purge_reset_after_event_id IS NOT NULL;
CREATE INDEX idx_purge_log_issue  ON purge_log(purged_issue_id);
CREATE INDEX idx_purge_log_lookup ON purge_log(project_name, issue_number);
CREATE INDEX idx_purge_log_issue_uid ON purge_log(issue_uid) WHERE issue_uid IS NOT NULL;
CREATE INDEX idx_purge_log_project_uid ON purge_log(project_uid) WHERE project_uid IS NOT NULL;
CREATE INDEX idx_purge_log_origin_instance ON purge_log(origin_instance_uid);

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT INTO meta(key, value) VALUES ('created_by_version', '0.1.0');

-- FTS5 virtual table over issue title+body+comments, kept in sync via AFTER INSERT/UPDATE/DELETE triggers.
CREATE VIRTUAL TABLE issues_fts USING fts5(
  title, body, comments,
  content='', tokenize='unicode61 remove_diacritics 2'
);
```

### 3.3 Event types

Persisted in `events.type` as fully qualified strings:

`issue.created`, `issue.updated`, `issue.closed`, `issue.reopened`, `issue.commented`, `issue.linked`, `issue.unlinked`, `issue.labeled`, `issue.unlabeled`, `issue.assigned`, `issue.unassigned`, `issue.soft_deleted`, `issue.restored`.

Plus the synthetic control event `sync.reset_required` (not persisted; emitted to live subscribers on purge and to reconnecting subscribers when their cursor falls inside a deleted-events window). Hook matchers (§8.3) and SSE `event:` fields use the same fully qualified names — there is no namespace shift between persistence, broadcast, and hooks.

The `issue.created` event payload includes initial `labels`, `links`, `owner`, `idempotency_key`, `idempotency_fingerprint` if any of those were specified at create time. No separate `issue.labeled`/`issue.linked`/`issue.assigned` events fire at creation. Subsequent changes do emit their own events.

### 3.4 Issue lifecycle

- `kata create` → row in `issues`, event `issue.created`. `projects.next_issue_number` bumped in same TX (`BEGIN IMMEDIATE`).
- `kata close [--reason …]` → `status='closed'`, `closed_at` set, default `closed_reason='done'`. Event `issue.closed`.
- `kata reopen` → `status='open'`, `closed_at` and `closed_reason` cleared. Event `issue.reopened`.
- `kata edit` → mutates `title`/`body`/`owner`/`priority` and link relationships in one atomic PATCH. Field changes emit `issue.updated`; priority emits `issue.priority_set` / `issue.priority_cleared`; link mutations emit a single aggregated `issue.links_changed` event per PATCH. _(Superseded §3.4 row; see `kata edit --help` for the shipped flag surface.)_
- `kata comment` → row in `comments`, event `issue.commented` with `{ "comment_id": N }`.
- ~~`kata link` / `unlink` (plus sugar verbs) → row in `links` or removal, event `issue.linked`/`issue.unlinked` with `related_issue_id` set on the event row.~~ _Removed in kata#1: link mutations live on `kata create` and `kata edit` as flag pairs (`--blocks` / `--remove-blocks`, `--parent` / `--remove-parent`, `--blocked-by` / `--remove-blocked-by`, `--related` / `--remove-related`). PATCH-driven changes emit one `issue.links_changed` event aggregating every edge; `issue.linked` / `issue.unlinked` are still emitted by the create-time initial link path._
- `kata label add/rm` → row in `issue_labels`, event `issue.labeled`/`issue.unlabeled`.
- `kata assign/unassign` → mutates `issues.owner`, event `issue.assigned`/`issue.unassigned`.

`updated_at` semantics: "last issue activity." Bumped by every event above.

### 3.5 Destructive ladder

1. `kata close` — closed but visible.
2. `kata delete <id>` — fails with hint *"deletion requires --force; use `kata restore` to undo."*
3. `kata delete <id> --force` — interactive prompt requires typing the issue number; sets `deleted_at`. Event `issue.soft_deleted`.
4. `kata restore <id>` — clears `deleted_at`. Event `issue.restored`.
5. `kata purge <id> --force` — interactive prompt requires typing exactly `PURGE #N`. In one TX:
   1. Capture `project_id`, `project_name`, `purged_issue_id` (the `issues.id` rowid), `issue_number`, `issue_title`, `issue_author`, and counts of dependent rows.
   2. **Capture** `events_deleted_min_id` / `events_deleted_max_id` from `SELECT MIN(id), MAX(id) FROM events WHERE issue_id = N OR related_issue_id = N` *before* deleting anything. Both are NULL if the issue has no events.
   3. Cascade-delete `events WHERE issue_id = N OR related_issue_id = N`, `comments`, `links`, `issue_labels`.
   4. If any events were deleted in step 3: bump `sqlite_sequence.seq` for `events` by one (`UPDATE sqlite_sequence SET seq = seq + 1 WHERE name = 'events'`) and capture the new value as `purge_reset_after_event_id`.
   5. Insert `purge_log` row with `project_id`, `purged_issue_id`, `project_name`, the captured fields and counts, `events_deleted_min_id`/`events_deleted_max_id` from step 2, and `purge_reset_after_event_id` from step 4 (NULL if no events deleted).
   6. Delete the `issues` row.
   7. After commit, the daemon broadcasts a `sync.reset_required` event over SSE with `id:` = `purge_reset_after_event_id` if that value is non-null. Live subscribers (with cursors below the reserved value) drop cache, refetch, and adopt the reserved id as their new cursor.

   The `purge_log` row is the only persisted record. **No `issue.purged` event is persisted.**

Both destructive verbs accept `--confirm "<exact-string>"` for noninteractive use, and require a TTY otherwise (else exit 6 `confirm_required`).

### 3.6 Idempotency

`POST /issues` with `Idempotency-Key: K`:

- Same key + same fingerprint → returns existing issue, `event=null`, `original_event=…`, `reused=true`. Exit 0.
- Same key + different fingerprint → exit 5 `idempotency_mismatch`.
- Same key + matched issue is soft-deleted → exit 5 `idempotency_deleted` with the deleted issue number; hint: `kata restore <id>` or use a fresh key.

**Fingerprint** covers every creation-affecting field so two requests with the same key but materially different inputs cannot silently reuse:

```
fingerprint = sha256(
    "title="   || canonical(title)        || "\n" ||
    "body="    || canonical(body)         || "\n" ||
    "owner="   || canonical(owner ?? "")  || "\n" ||
    "labels="  || join(",", sort(labels)) || "\n" ||
    "links="   || canonical(sort([{type, other_number} for each initial link]))
)
```

`canonical()` is NFC-normalized, trimmed of leading/trailing whitespace, with internal runs of whitespace collapsed to a single space (applied before hashing; the stored title/body remain verbatim). Initial links are sorted lexicographically by `(type, other_number)`. The two-element record uses a fixed JSON form for stability across language clients.

Idempotency key + fingerprint stored in the `issue.created` event's `payload`, indexed by `idx_events_idempotency`. Default lookback window 7 days (configurable).

### 3.7 Look-alike soft-block

Independent of idempotency. Pipeline:

1. FTS5 candidate retrieval over `(title, body, comments)`, top 20 by BM25.
2. App-level normalized similarity: tokenize, lowercase, stop-word, stem; Jaccard on title (weight 0.6) + Jaccard on first 500 chars of body (weight 0.4). Score in `[0, 1]`.
3. Soft-block when any candidate ≥ 0.7 (configurable).

Bypassed by `force_new=true` in the request body. **Idempotency wins** over `force_new` (idempotent reuse never emits a duplicate even if `force_new` is set).

## 4. Daemon HTTP API

### 4.1 Endpoint surface

```
GET    /api/v1/ping                                                # cheap liveness; no DB touch
GET    /api/v1/health                                              # deep health (DB, subscribers, uptime)
GET    /api/v1/instance                                            # local meta.instance_uid (federation discovery)

POST   /api/v1/projects                                            # body: {start_path, name?, replace?, reassign?}; daemon parses .kata.toml. used by `kata init`.
GET    /api/v1/projects                                            # list known projects
GET    /api/v1/projects/{project_id}                               # show project + aliases

POST   /api/v1/projects/resolve                                    # body: {name? or start_path}; daemon walks up for aliases/.kata.toml when no name is supplied;
                                                                   # fails if neither. used by every command except `kata init`.

POST   /api/v1/projects/{project_id}/issues                        # body includes actor, force_new
GET    /api/v1/projects/{project_id}/issues
GET    /api/v1/projects/{project_id}/issues/{number}
PATCH  /api/v1/projects/{project_id}/issues/{number}
GET    /api/v1/issues/{uid_or_prefix}                              # full UID or unique prefix (min 8 chars)

POST   /api/v1/projects/{project_id}/issues/{number}/actions/close
POST   /api/v1/projects/{project_id}/issues/{number}/actions/reopen
POST   /api/v1/projects/{project_id}/issues/{number}/actions/delete
POST   /api/v1/projects/{project_id}/issues/{number}/actions/restore
POST   /api/v1/projects/{project_id}/issues/{number}/actions/purge

POST   /api/v1/projects/{project_id}/issues/{number}/comments
POST   /api/v1/projects/{project_id}/issues/{number}/links
DELETE /api/v1/projects/{project_id}/issues/{number}/links/{link_id}
POST   /api/v1/projects/{project_id}/issues/{number}/labels
DELETE /api/v1/projects/{project_id}/issues/{number}/labels/{label}

GET    /api/v1/projects/{project_id}/ready
GET    /api/v1/projects/{project_id}/search?q=...
GET    /api/v1/projects/{project_id}/events?after_id=N&limit=N

GET    /api/v1/issues                                              # cross-project list
GET    /api/v1/events?after_id=N&limit=N                           # cross-project poll
GET    /api/v1/events/stream                                       # SSE; ?after_id or Last-Event-ID
```

All issue/project/link/event JSON shapes include additive UID fields. `Project` includes `uid`; `Issue` includes `uid` and `project_uid`; `LinkOut` includes `from_issue_uid` and `to_issue_uid`; event envelopes include `event_uid`, `origin_instance_uid`, `project_uid`, `issue_uid`, and `related_issue_uid` where applicable; `PurgeLog` envelopes include `uid` and `origin_instance_uid`. `event_uid` is a stable cross-instance identity but **not** a global ordering cursor — the ordered feed remains `events.id`. UIDs are never synthesized by exporters for an older source schema; importer fill rules add them while importing old JSONL into a current database.

### 4.2 Project resolution flow

CLI commands (other than `kata init`) call `POST /api/v1/projects/resolve` with either an explicit `name` from `--project <name>` or a `start_path` (typically cwd or `--workspace <path>`). The daemon walks upward to discover `W` (first `.kata.toml` ancestor) and `G` (first `.git` ancestor), then applies the resolution rules from §2.4.

Request body: `{ "name": "kata" }` or `{ "start_path": "/absolute/path" }`.

Successful response:

```json
{
  "project":        { "id": 7, "uid": "01JZ0000000000000000000001", "name": "kata", "next_issue_number": 42 },
  "alias":          { "id": 13, "alias_identity": "github.com/wesm/kata", "alias_kind": "git", "root_path": "/Users/wesm/code/kata" },
  "workspace_root": "/Users/wesm/code/kata"
}
```

Errors: `project_not_initialized` (404, no `.kata.toml` and no matching alias); `project_alias_conflict` (409, `.kata.toml` declares project P but the alias for the path is already attached to project Q ≠ P).

The CLI caches `project.id` per process and uses `/api/v1/projects/{id}/...` for subsequent calls in the same invocation.

`kata init` does **not** call `/projects/resolve`. It calls `POST /api/v1/projects` directly. Body fields:

- `start_path` (required): same start-path semantics; daemon walks up.
- `name` (optional): explicit project name from `--project`; if omitted and `.kata.toml` is present, the daemon reads the binding from the file; if neither, it derives from the git remote or local path.
- `replace` (optional bool): from `--replace`; permits overwriting an existing `.kata.toml` whose name disagrees with `name`.
- `reassign` (optional bool): from `--reassign`; permits moving an existing alias to this project.

The init endpoint is idempotent for the fresh-clone case (existing `.kata.toml`, no flags).

### 4.3 Auth model

- None. Loopback TCP / Unix socket only. The OS user is the trust boundary.
- Unix socket parent dir `0700`, socket `0600`.
- Reject any non-empty `Origin`. Require `Content-Type: application/json` on mutations. No CORS headers.

### 4.4 Required headers

| Header | Where | Purpose |
|---|---|---|
| `Idempotency-Key` | `POST /issues` | Optional. Daemon-side dedup with fingerprint check. |
| `X-Kata-Confirm` | `POST /actions/delete`, `/actions/purge` | Must equal `"DELETE #N"` / `"PURGE #N"` exactly. |
| `Last-Event-ID` | `GET /events/stream` | Standard SSE resume; `sync.reset_required` if cursor invalidated by purge. |
| `Accept: text/event-stream` | `GET /events/stream` | Required, else 406. |

### 4.5 Request/response shape

Every mutation request body includes `actor` (required, non-empty). Every successful mutation response includes:

```json
{
  "issue":   { "...": "full issue projection" },
  "event":   { "id": 81234, "type": "issue.created", "created_at": "..." },
  "changed": true,
  "reused":  false
}
```

No-op mutations always set `event: null, changed: false`:

- "Already in target state" cases (`label add` already labeled, `link add` already linked, `close` already closed, `reopen` already open) return `{ "issue": {...}, "event": null, "changed": false }`.
- **Idempotent reuse** is the named exception: returns `{ "issue": {...}, "event": null, "original_event": { "id": ..., "type": "issue.created", ... }, "changed": false, "reused": true }`. The `original_event` field is populated *only* in the idempotent-reuse case so clients can correlate to the prior creation.

### 4.6 Error envelope

Every non-2xx response:

```json
{
  "status": 409,
  "error": {
    "code": "duplicate_candidates",
    "message": "3 open issues match \"fix login\"",
    "hint": "comment on an existing issue, or pass force_new=true",
    "data": { "candidates": [...] }
  }
}
```

`error.code` is declared as an OpenAPI enum so generators emit stable Go constants. Huma error handler is wired so every non-2xx response uses this envelope, never Huma's default.

### 4.7 HTTP status → CLI exit code

| HTTP | CLI exit | Stable codes |
|---|---|---|
| 400 | 2 (usage) or 3 (validation) | `usage`, `validation`, `body_source_conflict`, `cursor_conflict` |
| 404 | 4 | `project_not_initialized`, `project_not_found`, `issue_not_found`, `link_not_found`, `label_not_found` |
| 409 | 5 | `duplicate_candidates`, `idempotency_mismatch`, `idempotency_deleted`, `parent_already_set`, `project_alias_conflict`, `project_binding_conflict` |
| 412 | 6 | `confirm_required`, `confirm_mismatch` |
| 500 | 1 | `internal` |
| (network) | 7 | (CLI maps `connection refused` → `daemon_unavailable`) |

### 4.8 SSE protocol

Endpoint: `GET /api/v1/events/stream[?project_id=N][?after_id=N]`. `Last-Event-ID` header alternative; both → 400 `cursor_conflict`.

Frame:

```
id: 81235
event: issue.commented
data: {"event_id":81235,"event_uid":"01J...","origin_instance_uid":"01J...","type":"issue.commented","project_id":3,"project_name":"kata","issue_number":42,"actor":"claude-4.7","payload":{"comment_id":104},"created_at":"2026-04-29T14:22:11.482Z"}
```

- `event:` field = `events.type` (e.g. `issue.commented`) or `sync.reset_required`. Same fully qualified strings used in `events.type` and hook matchers.
- `data:` is single-line JSON.
- Daemon broadcasts after DB commit; in-memory broadcaster fans out.
- On reconnect: compute `MAX(purge_reset_after_event_id) FROM purge_log WHERE purge_reset_after_event_id > <cursor>` (with `AND project_id = ?` for `?project_id=N` streams). If non-null → send single `sync.reset_required` (with `id:` = that max value, `data.reset_after_id` = same), close stream. Otherwise: replay `events WHERE id > ?` ordered by id (bounded ~10k rows; continue streaming live afterward).
- Heartbeats: `: keepalive\n\n` every 25s.

`sync.reset_required` event IDs are reserved synthetic cursors. They are produced by bumping `sqlite_sequence.seq` for `events` (without inserting a row) at purge time, so the value is strictly greater than every real `events.id` that existed at the moment of purge, and no real event will ever be assigned that id (the next real insert continues from `reserved + 1`).

### 4.9 Cross-project list

`GET /api/v1/issues` query params: `project_id` (repeatable), `status`, `owner`, `author`, `label` (repeatable), `q`, `updated_since`, `limit`, `offset`. Default sort `updated_at DESC`. Cursor pagination via `?after_updated=<ts>&after_id=<id>`.

### 4.10 Search response

```json
{
  "query": "fix login",
  "results": [
    { "issue": { "..." : "..." }, "score": 0.83, "matched_in": ["title","body"] }
  ]
}
```

Scoring server-side; same numbers everywhere (CLI, TUI).

### 4.11 Event polling (non-SSE)

`GET /api/v1/events?after_id=N&limit=L` and `GET /api/v1/projects/{project_id}/events?after_id=N&limit=L` are the polling counterparts to the SSE stream. They use the **same purge-invalidation rule** so an agent that polls cannot silently miss events.

For each request:

1. Compute `reset_to = MAX(purge_reset_after_event_id) FROM purge_log WHERE purge_reset_after_event_id > <after_id>`. The per-project endpoint adds `AND project_id = ?` (using the snapshotted `purge_log.project_id`); the cross-project endpoint omits the predicate.
2. **If `reset_to` is non-null** the cursor is invalidated. Return HTTP 200 with body:
   ```json
   {
     "reset_required": true,
     "reset_after_id": <reset_to>,
     "events": [],
     "next_after_id": <reset_to>
   }
   ```
   The `events` array is empty; the client refetches state and resumes polling with `after_id = reset_after_id`. (HTTP 200 keeps the response interpretable as a normal envelope; the `reset_required` flag is the trigger for the client.)
3. **Otherwise** return:
   ```json
   {
     "reset_required": false,
     "events": [ ...up to L envelopes, ordered by id ASC... ],
     "next_after_id": <max events.id in the response, or after_id if empty>
   }
   ```

The CLI text path treats `reset_required: true` the same way the TUI does on `sync.reset_required` over SSE: drop cached state, refetch, resume with the new cursor.

## 5. Agent Ergonomics & Skills

### 5.1 CLI conventions for agents

- `--json` is supported on **every** command (writes too). Stable schema, versioned (`{"kata_api_version":1, ...}`).
- Stable exit codes (table in §4.7); exposed as Go consts; man page entry `kata help exit-codes`.
- Body sources mutually exclusive: `--body`, `--body-file`, `--body-stdin`. Passing more than one → exit 2.
- With `--json`: missing body for `create` → empty body; for `comment` → exit 3 with hint. **Never opens `$EDITOR` in machine mode**, even if stdin happens to be a TTY.
- `--quiet, -q` suppresses non-essential output; compatible with `--json`. For `create`, `--quiet` without `--json` prints just the issue number.
- `kata events --tail --json` is **NDJSON** (one envelope per line).
- `kata events --after-id N --json` is the primary agent polling primitive; returns `next_after_id` for the next call. Timestamps (`--since`) are the human path. If the polling response sets `reset_required: true`, the agent must drop any cached state and resume polling from `next_after_id` (= the new baseline). See §4.11.
- `kata show` and relationship-command issue references accept `#N`, bare number `N`, full issue UID, or a unique UID prefix of at least 8 characters. Numbers remain project-scoped display labels; UIDs are the stable cross-cutover identity. Other lifecycle commands are still number-first until their parsers are moved onto the shared resolver.
- `kata export` / `kata import` are the supported schema-evolution and backup/restore path. JSONL exports preserve the source schema version exactly; imports into a newer binary apply deterministic fill rules, including UID generation for legacy records, inside a fresh database before validation and swap.
- **Project binding is workspace-driven, not flag-driven.** Agents do not pass `--project <identity>` on writes. They run from a workspace whose `.kata.toml` declares the binding. If `.kata.toml` is missing, write commands fail with `project_not_initialized` and a hint to run `kata init`. **Do not** auto-create a project — that is exactly how agents end up writing into the wrong namespace.

### 5.2 Identity (actor)

Precedence: `--as <name>` > `KATA_AUTHOR` > `$USER` > `git config user.name` > `anonymous`. `kata whoami` echoes the resolved identity and source (`flag`/`env`/`user`/`git`/`fallback`). `$USER` ranks above `git user.name` because login names (`wesm`) read more cleanly as event actors and owner tokens than display names with spaces (`Wes McKinney`).

Skills tell agents: set `KATA_AUTHOR` only when you need an actor distinct from `$USER` (e.g. an agent handle like `claude-4.7-wesm-laptop`). Otherwise the default is your shell login. Don't pass `--as` per-call unless acting as someone else.

### 5.3 Search before create

Two mechanisms working in concert:

1. **`kata search <query>`** — FTS5 + similarity score. Skills tell agents: always search before `create`.
2. **`kata create --idempotency-key <key>`** — if matched in the configured window, returns the existing issue with exit 0; with a different fingerprint, exit 5.

Look-alike soft-block at create time (≥ 0.7 similarity) errors with a candidate list; bypass with `--force-new`.

### 5.4 Error messages tell the agent what to do

Text mode:

```
$ kata create "fix login bug"
error: 3 open issues match "fix login" in this project
  #12  fix login bug on Safari       (open, 2d ago, claude-3.7)
  #18  login form crashes on submit  (open, 4h ago, codex)
  #22  login bug regression          (open, 1h ago, claude-4.7)
hint: comment on an existing issue, or pass --force-new to create anyway
```

JSON mode:

```json
{
  "error": {
    "code": "duplicate_candidates",
    "message": "3 open issues match \"fix login\"",
    "hint": "comment on an existing issue, or pass --force-new",
    "next_commands": ["kata show 12 --json", "kata show 18 --json", "kata show 22 --json"],
    "data": { "candidates": [{"number":12,"title":"...","score":0.81}, ...] }
  }
}
```

`project_not_initialized` text:

```
$ kata create "fix login bug"
error: kata project is not bound to this workspace
hint: run `kata init` (auto-derives the project name from git remote) or
      `kata init --project <name>` to attach to an existing project
```

### 5.5 Confirmation for destructive ops

Both `delete --force` and `purge --force` accept `--confirm "<exact-string>"` for noninteractive use. Agents in scripts use the flag; humans get the interactive prompt. Without TTY and without `--confirm` → exit 6 `confirm_required`. Mismatched `--confirm` → exit 6 `confirm_mismatch`.

The skill rule for agents: never invoke `kata delete` or `kata purge` unless the user explicitly named the issue number and instructed you to. Always include `--confirm` with the exact issue number.

### 5.6 Skill packaging

Directory-style. Embedded with `//go:embed`. Two targets v1:

```
~/.claude/skills/kata-using/SKILL.md      # honors $CLAUDE_CONFIG_DIR
~/.claude/skills/kata-triage/SKILL.md
~/.claude/skills/kata-decompose/SKILL.md

$CODEX_HOME/skills/kata-using/SKILL.md    # default ~/.codex
…
```

Each skill is a directory with at least `SKILL.md`; may include `references/`. Frontmatter is YAML with `name` and `description` — the description is the trigger phrase, written as roborev does it ("Use when …; do not use when …").

### 5.7 Skill set (3 skills, v1)

| Skill | Trigger | Content |
|---|---|---|
| `kata-using` | Foundation skill; install and prefer invoking when working in a kata-bound project | Identity, JSON-first, search-before-create, `.kata.toml` binding contract, link semantics, link/comment/close hygiene. Each skill has a "When NOT to invoke" section. |
| `kata-triage` | When user says "triage", "go through open issues", or similar — not for asking about a single issue | Walk `kata list --status open --json`, decide each: keep / close / link / comment. |
| `kata-decompose` | Large feature request — not for trivial requests | Parent issue + child issues with `parent` links, `blocks` chains for sequencing. |

Skills are 100–200 lines. Numbered steps, `--json` everywhere, explicit error handling ("if X fails, report and continue").

`kata-using` does **not** teach a `(kata-#N)` commit-message convention. Issue tracker is not git-history-derived.

`kata-using` instructs: if `kata create` returns `project_not_initialized`, do **not** retry with auto-create flags. Surface the error to the user; suggest they run `kata init`.

### 5.8 Verification commands

Agent skill activation isn't fully deterministic. Two backup commands:

- `kata skills doctor` — per-agent: installed / outdated / missing / skipped (config dir absent). Byte-compare installed content vs. embedded.
- `kata skills list` — names + descriptions; useful for verifying triggers loaded.
- `kata agent-instructions` — prints the canonical "what an agent should know about kata" text (same content shipped in `kata-using` SKILL.md). Deterministic fallback for when skills didn't load.

## 6. CLI Surface

Universal flags on every command: `--json`, `--quiet`/`-q`, `--as <name>`, `--workspace <path>` (overrides cwd as the workspace root used for resolution), and `--project <name>` (target an existing project by name from any directory). `--all-projects` for cross-project reads where applicable.

Note: there is no public `--project-id <N>` flag for agent-facing commands. Agents resolve through `.kata.toml`; the TUI and internal client may carry `project_id` end-to-end after a single `/projects/resolve` call but the value is never exposed as a CLI argument.

### 6.1 Command map

| Group | Command | Notes |
|---|---|---|
| Init | `kata init [--project <name>] [--replace] [--reassign]` | Fresh-clone flow: with no flags, reads existing `.kata.toml` if present; else derives name from git remote. Creates/upserts the project, attaches the workspace alias, writes `.kata.toml` if missing. The **only** path that creates project rows. Idempotent. |
| Lifecycle | `kata create [--project <name>] <title> [--body* / --idempotency-key K / --force-new / --label L / --owner O / --parent N / --blocks N]` | `--label` repeated only (no CSV). Initial labels/links/owner go into the `issue.created` event payload. Fails with `project_not_initialized` if no `.kata.toml`, `--project`, or matching alias. |
| | `kata show <issue-ref> [--include-events] [--include-deleted]` | Default: issue + comments + links + labels. `<issue-ref>` is `#N`, `N`, full UID, or unique UID prefix. |
| | `kata list [--status / --label / --owner / --author / --workspace / --all-projects / --updated-since / --limit / --search]` | Default: this project, `status=open`, `updated_at DESC`, limit 50. |
| | `kata edit <number> [--title / --body* / --owner / --priority] [--parent / --remove-parent] [--blocks / --remove-blocks] [--blocked-by / --remove-blocked-by] [--related / --remove-related]` | At least one field/relationship; else exit 3. Atomic PATCH; relationship flags are repeatable except `--parent`. Refs accept `#N`, `N`, full UID, or 8+ char UID prefix. _(Updated in kata#1.)_ |
| | `kata close <number> [--reason done\|wontfix\|duplicate]` | Default reason `done`. |
| | `kata reopen <number>` | |
| | `kata comment <number> [--body*]` | Body required (no implicit empty comment). |
| | `kata delete <number> --force [--confirm "DELETE #N"]` | Soft delete; reversible. |
| | `kata restore <number>` | |
| | `kata purge <number> --force [--confirm "PURGE #N"]` | Irreversible. |
| Relationships | _Removed in kata#1._ The dedicated `parent` / `unparent` / `block` / `unblock` / `relate` / `unrelate` / `link` / `unlink` commands have all been retired in favor of relationship flags on `kata create` and `kata edit` — see the rows for those commands above; `kata edit --help` documents the current flag surface. |
| Labels | `kata label add <number> <label>` / `kata label rm <number> <label>` | Charset `[a-z0-9._:-]{1,64}`. |
| | `kata labels [--workspace / --all-projects]` | Counts. |
| Ownership | `kata assign <number> <owner>` / `kata unassign <number>` | |
| Discovery | `kata ready [--workspace / --all-projects / --label / --owner]` | Open issues with no open `blocks` predecessor. **Primary "what's next" command.** |
| | `kata search <query> [--workspace / --all-projects / --status / --limit]` | FTS5 + similarity. |
| | `kata events [--after-id / --since / --tail / --workspace / --all-projects / --type]` | Default: this project, 100 most recent. `--tail` → NDJSON over SSE. |
| Diagnostics | `kata doctor [--workspace / --all-projects]` | Read-only; system health only (see §6.4). |
| | `kata whoami` | `{actor, source}`. |
| | `kata health` | `/api/v1/health`. |
| Projects | `kata projects [list\|show]` | List known projects (with their aliases) / show one. |
| Backup | `kata export [--output path] [--project-id N] [--allow-running-daemon]` | Writes a git-friendly JSONL database export. |
| | `kata import --input path --target path [--force]` | Imports JSONL into a fresh target DB; validates before commit. |
| Skills | `kata skills install [--target claude\|codex\|all]` | Idempotent; honors `$CLAUDE_CONFIG_DIR`/`$CODEX_HOME`. |
| | `kata skills doctor` / `kata skills list` | |
| | `kata agent-instructions` | Canonical agent doc. |
| Daemon | `kata daemon [start\|stop\|status\|logs\|reload]` | `logs --hooks` for hook runs. Auto-start by other commands. |
| TUI | `kata tui [--workspace / --all-projects / --include-deleted]` | (see §7) |
| Config | `kata config [get\|set\|list\|path]` | TOML. |

### 6.2 Project resolution (per command)

Every command except `kata init` follows this flow:

1. Determine the start path: `--workspace <path>` if given, else `cwd`.
2. If `--project <name>` is present, CLI sends `POST /api/v1/projects/resolve { "name": <name> }` to the daemon.
3. Otherwise CLI sends `POST /api/v1/projects/resolve { "start_path": <start_path> }` to the daemon.
4. Daemon walks upward to find `W` (first `.kata.toml` ancestor) and `G` (first `.git` ancestor). Resolution per §2.4: alias lookup; else `.kata.toml` name; else fail with `project_not_initialized` (exit 4).
5. CLI uses returned `project.id` for subsequent calls in this invocation.

`--all-projects` short-circuits the resolve step; the command fans out across registered projects.

`kata init` does **not** call `/projects/resolve`. It calls `POST /api/v1/projects` directly with the same start-path-driven body shape; the daemon owns `.kata.toml` parsing and name derivation. After init, every command in the workspace resolves through its alias or `.kata.toml`; commands can also target another project with `--project <name>`.

### 6.3 `.kata.toml` format

Committed at the workspace root. Declarative binding only — no mutable state, no caches.

```toml
version = 1

[project]
name = "kata"
```

Fields:

- `version` (required, int): config schema version. v1 = 1. Future-proofing.
- `project.name` (required, string): the project name and lookup key.

Future versions may add an optional `[hooks]` block and `[[hook]]` entries. v1 omits these (see §10).

If `.kata.toml` is malformed, the resolve endpoint returns exit 3 `validation` with the parse error.

### 6.4 Detailed: `kata create`

Most-hit command. JSON output is the full issue projection plus event metadata:

```json
{
  "issue":   { "number": 42, "...": "..." },
  "event":   { "id": 81237, "type": "issue.created" },
  "changed": true,
  "reused":  false
}
```

Daemon flow inside one TX: resolve project (→ exit 4 if not initialized) → idempotency check → look-alike check → insert issue + initial labels/links → bump `projects.next_issue_number` → append `issue.created` event with payload (idempotency, fingerprint, initial labels/links/owner) → commit → broadcast.

### 6.5 Detailed: `kata doctor` (system-only)

Read-only. JSON output is an array of findings; text output groups by severity.

| Check | Severity | Notes |
|---|---|---|
| `daemon_unreachable` | error | `/ping` fails. |
| `db_integrity_failed` | error | `PRAGMA integrity_check`. |
| `schema_drift` | warn | DB `meta.schema_version` vs. binary's expected version. |
| `runtime_files_stale` | warn | `daemon.<pid>.json` for non-existent PIDs. |
| `config_parse_error` | warn | Global config TOML failed to load. |
| `purge_log_inconsistency` | warn | For each `purge_log` row, no `events` row should have `issue_id = purged_issue_id` or `related_issue_id = purged_issue_id` (cascade missed rows). Uses the captured rowid rather than `project_name + issue_number` so project renames can't mask stale events. Reports per offending event. |
| `skill_install_drift` | warn | Per agent: missing/outdated skills (byte-compare). |

Doctor never recommends workflow mutations. It may recommend system-repair commands like `kata daemon reload`, `kata skills install`, or "remove stale runtime files at <paths>".

### 6.6 Detailed: `kata ready`

```sql
SELECT i.* FROM issues i
WHERE i.project_id = ? AND i.status = 'open' AND i.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM links l
    JOIN issues blocker ON blocker.id = l.from_issue_id
    WHERE l.type = 'blocks' AND l.to_issue_id = i.id
      AND blocker.status = 'open' AND blocker.deleted_at IS NULL
  )
ORDER BY i.updated_at DESC;
```

Default sort `updated_at DESC`. No `--by priority-label` flag (label-taxonomy bias); filter via `--label priority:high` instead.

### 6.7 Config

`$KATA_HOME/config.toml` (or `~/.kata/config.toml`).

```toml
# server_addr = "unix:///custom/path/daemon.sock"
# server_addr = "127.0.0.1:7474"

similarity_threshold = 0.7
idempotency_window   = "7d"

[hooks]
max_concurrency = 4
queue_size      = 1000

# hook entries in $KATA_HOME/hooks.toml (separate file)
```

Workspace-local config (`<workspace>/.kata.toml`) carries only the project binding in v1, not behavior overrides.

## 7. TUI

Bubble Tea + Lipgloss. Single command: `kata tui`. API-only; no SQLite bypass; writes go through REST like any other client.

### 7.1 Views

- **List** (default landing): issues, filter/search inline (`/`), status/label/owner toggles.
- **Detail**: header + body + tabs `[ comments | events | links ]`.
- **Help** (`?`): keybindings, current filters, daemon health line.

### 7.2 Scope

- `kata tui` → current project (resolved from `cwd` via `.kata.toml` / alias).
- `kata tui --all-projects` → cross-project, project column shown in list.
- Toggle at runtime with `R`.
- **Outside a project-bound workspace**: if any projects are registered, fall back to all-projects automatically. If none registered, show clean empty-state with hint *"Run `kata init` in a repo to get started."*
- `kata tui --include-deleted` shows soft-deleted rows with a `[deleted]` marker. Without the flag, deleted rows are entirely hidden.

### 7.3 Keybindings

**Global**: `?` help · `q` quit · `R` toggle scope (project/all). (`:` is unbound; reserved for a future command palette — see out-of-scope.)

**List**: `j/k`, arrows, `g/G`, `enter` open · `n` new (inline title prompt → optional `$EDITOR` for body) · `/` search · `s` cycle status filter · `o` filter by owner · `l` filter by label · `c` clear filters · `x` close · `r` reopen.

**Detail**: `j/k` scroll · `tab/shift-tab` cycle tabs · `enter` on an event referring to another issue → jump · `c` new comment (suspend Bubble Tea, run `$EDITOR`, resume, submit) · `e` edit body (same flow) · `x` close · `r` reopen · `p` set parent · `b` add blocker · `L` add link · `+`/`-` add/remove label · `a`/`A` assign/clear owner · `backspace`/`esc` back.

**Destructive ops** (`delete`, `purge`) intentionally **not** keybound. Use the CLI.

### 7.4 SSE behavior — invalidation, not replication

On any incoming event:

- Mark affected row(s) stale and schedule a debounced (~150ms) refetch of the active list query.
- If the detail view is showing the affected issue, refetch that issue.
- On `sync.reset_required`: drop cache, refetch current view, reopen stream with new cursor; show `resynced` toast for ~2s.

Reconnect on disconnect with exponential backoff to 30s; status bar shows `daemon: reconnecting…`.

### 7.5 Color

`KATA_COLOR_MODE` ∈ `auto` (default) / `dark` / `light` / `none`. `NO_COLOR=1` honored. Single theme v1; Lipgloss adaptive.

### 7.6 Performance

Responsive on a few thousand issues. Cold start to first paint and active-view refetch are tested with synthetic fixtures. No microsecond promises in v1.

## 8. Hooks

### 8.1 Goals

Local automation on `issue.*` events. Common use cases: post to chat, file follow-up GitHub issues, ping a notification daemon, log to disk. No remote webhooks v1.

### 8.2 Config

Single global location in v1: `$KATA_HOME/hooks.toml`. Workspace-local hook config (e.g., `<workspace>/.kata.toml [[hook]]` blocks) is out of scope for v1 and listed in §10.

```toml
[[hook]]
event   = "issue.created"           # exact event type, "issue.*", or "*"
command = "/usr/local/bin/notify"   # absolute path or PATH-resolvable name
args    = ["--title", "kata"]       # literal strings; no env-var expansion
timeout = "30s"                     # default 30s, max 5m

[hook.env]
EXTRA = "value"                     # user env; keys matching ^KATA_ rejected at load
```

Optional fields: `working_dir` (absolute path; default `$KATA_HOME`).

`sync.reset_required` is **not** dispatched to hooks. Hooks see persisted domain events only.

### 8.3 Event matching

Hook `event` strings are fully qualified — same names used in `events.type` and SSE `event:` fields.

- Exact: `event = "issue.commented"`.
- Prefix wildcard: `event = "issue.*"` matches all `issue.<verb>`.
- Catch-all: `event = "*"` matches everything except `sync.reset_required` (which is never dispatched to hooks).

### 8.4 Stdin payload

```json
{
  "kata_hook_version": 1,
  "event_id": 81237,
  "type": "issue.commented",
  "actor": "claude-4.7-wesm-laptop",
  "created_at": "2026-04-29T14:22:11.482Z",
  "project": {
    "id": 3,
    "name": "kata"
  },
  "alias": {
    "alias_identity": "github.com/wesm/kata",
    "alias_kind": "git",
    "root_path": "/Users/wesm/code/kata"
  },
  "issue": {
    "number": 42,
    "title": "fix login crash on Safari",
    "status": "open",
    "labels": ["bug","safari"],
    "owner": "claude-4.7-wesm-laptop",
    "author": "claude-4.7-wesm-laptop",
    "_truncated": false
  },
  "payload": { "comment_id": 104, "comment_body": "..." }
}
```

The `alias` block snapshots the workspace alias whose action emitted the event (the alias that resolved during the originating CLI call). For events emitted from contexts without a single alias (rare; e.g., admin paths in future plans), `alias` may be omitted.

Hook stdin is JSON, capped at 256KB. Large text fields (`issue.title`, `issue.body`, `payload.comment_body`) are truncated with sibling `_truncated: true` and `_full_size: N` markers. Hooks needing full content fetch via `kata show <number> --json`.

### 8.5 Env vars (safe scalars only)

```
KATA_HOOK_VERSION, KATA_EVENT_ID, KATA_EVENT_TYPE, KATA_ACTOR, KATA_CREATED_AT,
KATA_PROJECT_ID, KATA_PROJECT_NAME, KATA_ISSUE_NUMBER,
KATA_ALIAS_IDENTITY, KATA_ROOT_PATH                   # alias-scoped; absent for admin-emitted events
```

User-defined `env` entries layer first; reserved `KATA_*` set last. Config rejects `env` keys matching `^KATA_` at load with a clear error. Issue title/body/comment text is **never** in env.

### 8.6 Execution

- **After DB commit.** Hooks never block or roll back state changes.
- `exec.Command(cmd, args...)`. **No shell.** **No env-var expansion in `args`.** Hook commands run with the daemon's UID/GID and inherit no kata internals beyond the documented `KATA_*` env vars; this avoids the obvious shell-injection surface but is **not** a sandbox. Operators who need OS-level isolation should write hooks that re-exec under their own sandboxing (containers, `firejail`, `bwrap`, etc.).
- Bounded hook-runner pool: default 4 goroutines (configurable, capped at 16). Bounded queue default 1000.
- Per-hook timeout. SIGTERM → 5s grace → SIGKILL.
- Capture stdout/stderr to `$KATA_HOME/hooks/<dbhash>/output/<event_id>.<hook_index>.{out,err}`. Total disk usage capped (default 100MB per `<dbhash>`; oldest files pruned first; configurable via `[hooks].output_disk_cap`).
- Hook-run index: `$KATA_HOME/hooks/<dbhash>/runs.jsonl` (rotated 50MB × 5). One JSON object per run with start/end times, exit code, timeout flag, stdout/stderr paths, truncation flag. Used by `kata daemon logs --hooks`.

`<dbhash>` namespacing prevents two `KATA_DB` daemons from interleaving event-ID-keyed output files or sharing one `runs.jsonl`.

### 8.7 Reload

- `kata daemon reload` (or SIGHUP) reloads global config.
- Validation at load: required fields, `timeout` parses and is in `(0, 5m]`, `working_dir` (if set) parses correctly, `env` keys valid (no `KATA_`).
- **Not validated at load**: command on PATH, command file existence. PATH may differ at fire time. Spawn errors get recorded per run.

### 8.8 Failure visibility

- `kata daemon logs --hooks` tails `runs.jsonl` (last 100 by default). `--tail` follows live. `--failed-only`, `--event-type`, `--hook-index` filters.
- Hook failures are **not** SSE events and **not** doctor findings (system health only).
- **Queue full** drop policy: drop newest. Log `hook_queue_full` once per 60s to `daemon.log`. Event handling and SSE broadcast unaffected.
- **`working_dir` missing** at fire time: run recorded as `failed: working_dir_missing`; logged once per 60s in `daemon.log` to avoid spam.

## 9. Filesystem Layout & Project Structure

### 9.1 Filesystem layout

**Data dir** (precedence: `KATA_HOME` → `~/.kata`; `KATA_DB` overrides DB path independently):

```
$KATA_HOME/
  config.toml
  hooks.toml
  kata.db                              # default; override KATA_DB
  kata.db-wal
  kata.db-shm
  hooks/<dbhash>/
    runs.jsonl
    output/<event_id>.<hook_index>.{out,err}
  runtime/<dbhash>/
    daemon.<pid>.json
    daemon.log
```

**Runtime dir** (ephemeral; socket only):

```
$XDG_RUNTIME_DIR/kata/<dbhash>/        # fallback: $TMPDIR/kata-<uid>/<dbhash>/
  daemon.sock                          # 0600; parent 0700
```

`<dbhash>` = `sha256(absolute(effective_db_path))[:12]`.

**Workspace files** (committed):

```
<workspace_root>/.kata.toml            # required; declares project binding (§6.3)
```

There are no other workspace-local kata files in v1. No `.kata/` directory, no per-workspace caches, no per-workspace hooks.

### 9.2 Go project layout

```
cmd/kata/
  main.go
  {create,show,list,edit,close,reopen,comment,delete,restore,purge}.go
  {parent,unparent,block,unblock,relate,unrelate,link,unlink}.go
  {label,labels,assign,unassign,ready,search,events}.go
  {whoami,health,init,projects,doctor}.go
  {daemon_cmd,skills,agent_instructions,tui_cmd,config_cmd}.go
  helpers.go                           # CLI glue: body-source reader, JSON formatter, exit codes
  testmain_test.go
  tui/
    tui.go fetch.go handlers.go render_list.go render_detail.go filter.go theme.go

internal/
  api/
    routes.go            # huma route registration (source of truth)
    types.go             # request/response DTOs
    errors.go            # error envelope + Huma wiring
    openapi.yaml         # generated artifact (committed for diff)
  apiclient/generated/
    client.gen.go        # generated from openapi.yaml
  daemon/
    runtime.go           # daemon.<pid>.json, ListAllRuntimes, cleanup
    endpoint.go          # DaemonEndpoint (Unix vs TCP)
    namespace.go         # dbhash, runtime dir resolution
    server.go            # http.Server lifecycle, signal handling
    broadcaster.go       # SSE fan-out, Last-Event-ID resume, sync.reset_required
    hooks/
      runner.go          # worker pool, queue, exec
      config.go          # TOML load, validation
      log.go             # JSONL index + rotated stdout/stderr capture
      payload.go         # build stdin JSON, truncation
    health.go            # /health, /ping
    handlers_{projects,issues,events,actions,labels,links,comments,search,ready}.go
  db/
    db.go                # Open (pragmas, FK enforcement), migrations runner
    migrations/0001_init.sql
    queries.go           # all CRUD, single-writer aware
    types.go             # Issue, Comment, Link, Label, Event, PurgeLog, Project, ProjectAlias
    fts.go               # FTS5 virtual table + sync triggers
  config/
    config.go            # global TOML
    project_config.go    # parse .kata.toml from a workspace root
    project_identity.go  # alias_identity computation (git remote → normalized; local://)
    paths.go             # KATA_HOME, KATA_DB, runtime dir, workspace discovery (used by CLI too)
  similarity/
    similarity.go        # tokenize, normalize, jaccard, weighted score
  skills/
    skills.go            # install, status, doctor; CLAUDE_CONFIG_DIR / CODEX_HOME
    claude/{kata-using,kata-triage,kata-decompose}/SKILL.md
    codex/{kata-using,kata-triage,kata-decompose}/SKILL.md
  testenv/testenv.go     # temp data dir, fresh daemon, generated client wired
  testutil/testutil.go   # git temp repos, fixtures
```

**Project resolution lives in `internal/config/`**: `project_identity.go` derives alias identities (the deterministic alias_identity for a workspace path), `project_config.go` parses `.kata.toml`. The daemon's `handlers_projects.go` composes them with DB lookup. CLI sends `name` for `--project <name>` or `start_path` for workspace resolution.

### 9.3 Build, test, lint

```
make build            # go build ./...
make install          # GOBIN=~/.local/bin go install ./cmd/kata
make test             # go test -shuffle=on ./...
make test-short       # go test -short -shuffle=on ./...
make lint             # golangci-lint run --config .golangci.yml
make vet              # go vet ./...
make api-generate     # huma routes → openapi.yaml → apiclient
```

API generation has one source of truth: Huma route/type registrations. `make api-generate` runs an internal generator that exports the spec to `internal/api/openapi.yaml` (committed for review/diff), then generates `internal/apiclient/generated/` from that file.

Conventions: testify preferred (`require` for setup, `assert` for non-blocking); table-driven tests; `t.TempDir()`; `-shuffle=on`; no `-count=1`; no `-v` by default. `modernc.org/sqlite`; no CGO. UTC timestamps; RFC3339 at API boundaries. Schema timestamp columns typed `DATETIME`. No emojis in code or output.

Pre-commit via `prek` (matches roborev/middleman): runs `make lint` with `always_run`. CI uses `make lint-ci` non-mutating.

## 10. Out-of-Scope (Consolidated)

**Storage / DB:**
- Backwards-compat `daemon.json` alias.
- Direct SQLite reads from CLI (separate `OpenReadOnly` later).
- Importers (GitHub Issues, beads, JIRA).
- PostgreSQL mirror / multi-machine sync.
- Project / alias deletion. (Aliases accumulate; in v2, `kata projects forget <id>` and `kata projects detach <alias>` may land.)

**API:**
- Multi-user auth, agent tokens.
- GraphQL/RPC.
- Bulk endpoints.
- Per-issue SSE subscriptions (clients filter).
- Remote webhooks.
- Diagnostic admin path for project registration outside `kata init`.
- Auto-create project on first read/write. (The strict policy is intentional; `kata init` is the only path.)

**CLI:**
- `kata sync`, `kata import`.
- `kata link-commits` and the `(kata-#N)` commit-message convention.
- `kata report` / `kata hygiene` (workflow checks).
- Interactive shell mode.
- Project aliases / "previous project" sugar.
- `--watch` on individual list/show commands (use `kata events --tail`).
- `kata projects forget` / `kata projects detach` (deferred to v2).
- `--project-id <N>` flag on agent-facing commands. (TUI/internal only.)

**Workspace config:**
- Workspace-local hooks (`<workspace>/.kata.toml [[hook]]` blocks). `.kata.toml` v1 carries only the project binding.
- Workspace-local behavior overrides (similarity threshold, idempotency window, etc.).

**TUI:**
- Command palette (`:` reserved unbound).
- Multi-pane / split layout.
- Parent/child tree visualization.
- Bulk operations / multi-select.
- Markdown rendering.
- OS notifications (delegate to hooks).

**Hooks:**
- Remote webhooks.
- Retries.
- Conditional firing (`when = …`).
- Ordering guarantees.
- SSE-driven external triggers.
- Workspace-local hook configs.

**Skills:**
- Auto-installation on first daemon start.
- Skill marketplace / external registries.
- Per-project skill overrides.

**Doctor:**
- Workflow lints (stale opens, dangling owners, commit-ref orphans).
- Auto-applied fixes — doctor only *recommends* commands. Recommendations are limited to system-repair commands like `kata daemon reload`, `kata skills install`, or "remove stale runtime files at <paths>"; never workflow mutations.

**Issue model:**
- `in_progress` status (use labels or `owner`).
- Draft / needs-review states.
- Issue templates.
- Severity / priority as first-class fields (use labels).
- Threaded comment replies.
- Reactions / emoji.
- File attachments.

## 11. Open Questions / Tunables

- **Default similarity threshold 0.7** — empirical default. Calibrate against a fixture set of ~50 positive/negative issue-pair examples during initial implementation.
- **Default idempotency window 7 days** — long enough to catch flake-retry duplicates; short enough that intentional re-creates aren't blocked.
- **`--label` flag** is repeated only (`--label bug --label safari`); no CSV.
- **`--json` create output** is the full issue projection. `--quiet` (without `--json`) prints just the number for scripting.
- **Skill set v1**: `kata-using`, `kata-triage`, `kata-decompose`. A fourth "land/finish" skill was considered and dropped as imported process; add it later only if real usage shows it earns a skill.

---

End of v1 spec.
