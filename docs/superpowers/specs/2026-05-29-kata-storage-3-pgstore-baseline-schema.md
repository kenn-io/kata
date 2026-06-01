# Storage Phase 3 — pgstore baseline schema

> Phase 3 of the kata Postgres-backend work (parent spec `docs/superpowers/specs/2026-05-26-kata-postgres-backend.md` §3, §5). Phase 1d landed `db.Storage` + `sqlitestore` + `storeopen`; Phase 2 landed the migration runner with `Storage.Migrate` and the `kata migrate` CLI; Phase 8 added `KataDSN` resolution and `[storage].dsn` config plumbing. Phase 3 lays down the Postgres schema at v10 — tables, constraints, triggers, indexes, and the FTS surface — without implementing any of pgstore's query methods. Schema only; queries arrive in Phase 4.

## Goal

`internal/db/pgstore/migrations/0010_baseline.sql` ships a Postgres schema semantically equivalent to today's SQLite `0010_baseline.sql`. A fresh Postgres database, opened via `storeopen.Open(ctx, "postgres://…", db.ApplyMigrations())`, reaches `meta.schema_version = 10` (then `11` after the new idempotency-uniqueness migration also lands on the SQLite side). The `pgstore` package exposes only what Phase 3 needs: the embedded migrations FS, a `Store` shell that satisfies `db.Storage` enough to drive `Migrate` (no domain methods yet — those are stubs returning a clear "not implemented in Phase 3" error so the package compiles against the interface), and the package-level glue that lets `storeopen.Open` dispatch postgres DSNs to it.

Phase 3 also ships a sqlitestore companion migration: `internal/db/sqlitestore/migrations/0011_idempotency_unique.sql` adds a UNIQUE partial index that gives the SQLite side the same idempotency-race closure as the Postgres baseline. This bumps `db.CurrentSchemaVersion()` from 10 to 11.

## Non-goals (deferred to Phases 4–7)

- pgstore CRUD / search / export / import method implementations (Phase 4 + 5).
- The cross-backend conformance test suite + testcontainers harness (Phase 6).
- The `kata migrate-backend` command (Phase 7).
- DSN/config wiring (already landed in Phase 8).
- Migration of existing user data into Postgres. Phase 3 only stands up the schema on fresh PG databases.

## Background (from parent spec)

- **Parent spec §3** mandates the migration runner model: each backend embeds `migrations/*.sql`; `0010_baseline.sql` is the consolidated v10 schema. The runner advances `meta.schema_version` per applied file. Postgres has no pre-v10 history; its baseline IS v10.
- **Parent spec §5** inventories the SQLite-isms in `schema.sql` and pins each translation: `BIGINT GENERATED ALWAYS AS IDENTITY` for surrogate keys, `TEXT` (not `TIMESTAMPTZ`) for `*_at` columns to preserve byte-identical JSONL roundtrip, `TEXT` (not `jsonb`) for JSON columns for the same reason, `CHECK ((x)::jsonb IS NOT NULL)` for JSON validity, `(name NOT LIKE '%#%')` for the project-name glob, POSIX regex for the short_id character set, PL/pgSQL trigger functions replacing six SQLite `RAISE(ABORT, …)` triggers, and `tsvector` + GIN replacing FTS5.
- **Parent spec §4** flags `events.idempotency_key`'s check-then-insert race: today's `idx_events_idempotency` is non-unique; SQLite's single-writer is what closes the race in practice. Phase 3 closes it at the schema level for both backends.
- **Parent spec §5.1** ranks search as the highest-risk port. tsvector ordering (`ts_rank` DESC) cannot match FTS5 `bm25` exactly; Phase 6 conformance tests assert membership/relevance bands plus a deterministic `id ASC` tiebreaker, not exact rank-equivalent order.

## Design

### Architecture: pgstore as sqlitestore's structural twin

`internal/db/pgstore` mirrors `internal/db/sqlitestore`'s package shape so future work (queries, search, export/import) lands in symmetric files. Phase 3 only fills in the migration surface + the minimum needed for `Migrate` to run:

```
internal/db/pgstore/
  migrations/
    0010_baseline.sql              # the v10 schema (tables, constraints, triggers, indexes, FTS, UNIQUE idempotency index)
    0011_idempotency_unique.sql    # placeholder so the runner stamps schema_version=11 in lockstep with sqlitestore — body is a comment + SELECT 1
  store.go                 # Store shell: *sql.DB, Path(), Close(), InstanceUID(), readOnly flag, PeekSchemaVersion
  open.go                  # Open(ctx, dsn, opts...) — opens via pgx stdlib driver, applies per-connection runtime params (statement_timeout, etc.)
  migrate.go               # Store.Migrate impl: pg_advisory_xact_lock, embed.FS ladder, ON CONFLICT DO UPDATE stamping
  migrations_export_test.go  # SetMigrationsSource + EmbeddedMigrationsFS test seam (same shape as sqlitestore)
  schema_test.go           # CREATE-table presence + constraint shape assertions, run against a testcontainers PG instance
  migrate_test.go          # at-current noop, synthetic 11+, rollback, concurrent
```

Domain-method stubs (`CreateIssue`, `CreateProject`, `AddLabel`, …) all return `errors.New("pgstore: not implemented in Phase 3 — see Phase 4 for queries")` and are wired up only so `*pgstore.Store` satisfies `db.Storage`. Each stub has a `// TODO(phase-4):` comment naming the entity group. The stubs are intentionally noisy to make any accidental Phase-3 caller fail fast.

**Driver choice.** Use `github.com/jackc/pgx/v5/stdlib` (pgx's database/sql wrapper) so the standard `database/sql` API surface still applies and the `*sql.DB` pool semantics match sqlitestore. `pgx` is the de-facto Go pg driver; no project currently depends on `lib/pq`, which is in maintenance mode.

**Connection pool defaults.** `SetMaxOpenConns(25)`, `SetMaxIdleConns(5)`, `SetConnMaxIdleTime(5 * time.Minute)`. Tunable through DSN query params (`pool_max_conns`, `pool_max_idle_conns`, `pool_idle_lifetime`) parsed by pgstore.Open. SQLite's single-writer reality has no pool to tune; pgstore needs reasonable defaults that don't surprise a single-daemon deployment.

**Driver-level settings on every connection.** A `connConfigSetup` hook (registered through pgx's `BeforeConnect`) sets `application_name='kata'`, `statement_timeout='30s'`, and `idle_in_transaction_session_timeout='60s'` on every new connection. The defaults are conservative; operator-overridable via DSN.

### `Storage.Migrate` for Postgres

Mirror sqlitestore.Migrate's shape — same entry guards, same ladder validation, same `MigrationResult` semantics — with Postgres-specific concurrency:

- **Lock**: `pg_advisory_xact_lock(hashtextextended($1, 0))` where `$1` is a deterministic constant `'kata:migrate'`. Held for the duration of the migration transaction. Concurrent migrators block until release; Postgres's lock manager fairly serializes them.
- **Snapshot**: no equivalent. Phase 3 spec acknowledges this gap. Operators are expected to use base-backup or `pg_dump` before running `kata migrate` on a Postgres deployment. The Migrate error path documents this in the "snapshot retained at" message by omitting the snapshot suffix on PG (the message ends with `(no Postgres snapshot — restore via your operator backup)` instead of a path).
- **Transaction**: one `BEGIN` for the whole apply loop; per-file `tx.ExecContext`; final `meta.schema_version` upsert; `COMMIT`. On any per-step error, rollback. The pinned `*sql.Conn` pattern (used by sqlitestore for `BEGIN IMMEDIATE`) is unnecessary here — Postgres transactions are connection-scoped by default and pgx surfaces them through `*sql.Tx`.
- **Entry guards**: identical to sqlitestore — reject `current > 0 && current < db.BaselineSchemaVersion` (no pre-baseline DBs exist on Postgres in practice, but the guard prevents accidentally pointing a fresh kata at a corrupted PG DB someone tagged with `meta.schema_version=5`) and reject `current > db.CurrentSchemaVersion()` (newer-than-binary).
- **`instance_uid`**: same flow as sqlitestore — `cacheInstanceUIDIfPresent` on Open; Migrate's pending-work path stamps via `ensureInstanceUIDOnConn`; the no-pending path's `ensureInstanceUIDFromMeta` repairs a missing row using `INSERT ... ON CONFLICT DO NOTHING`.
- **Ladder validation**: identical to sqlitestore — `listPendingMigrations` rejects duplicate version prefixes, gaps, and files above `db.CurrentSchemaVersion()`. The helper itself is duplicated for now; a follow-up could lift it into a backend-neutral helper, but Phase 3's blast radius is small.

### Schema (the actual port from §5)

`0010_baseline.sql` opens with extension and search-config setup, then declares tables in FK-dependency order (parents before children), then indexes, then triggers, then meta. The meta seed (`schema_version='10'`, fresh `instance_uid`) is left to `Migrate`'s stamping logic — the migration file itself does not INSERT into meta, matching sqlitestore's pattern.

**File-top setup:**

```sql
CREATE EXTENSION IF NOT EXISTS unaccent;

-- Custom text search config: unaccent over simple. Same lower-no-stem
-- tokenization as SQLite's `unicode61 remove_diacritics 2`.
DROP TEXT SEARCH CONFIGURATION IF EXISTS kata_simple_unaccent;
CREATE TEXT SEARCH CONFIGURATION kata_simple_unaccent (COPY = simple);
ALTER TEXT SEARCH CONFIGURATION kata_simple_unaccent
  ALTER MAPPING FOR hword, hword_part, word
  WITH unaccent, simple;
```

**Table order (per §5's forward-FK note):** `meta`, `projects`, `project_aliases`, `recurrences`, `issues`, `comments`, `links`, `import_mappings`, `events`, `purge_log`, `issue_labels`, `issues_search`, `api_tokens`. Tables that reference others are declared after their targets. Where this forces a divergence from SQLite's declaration order (notably `recurrences` before `issues`), the equivalent SQLite-side files are unaffected — only the PG baseline reorders.

**Type translations (verbatim from §5):**

| SQLite | Postgres |
|---|---|
| `INTEGER PRIMARY KEY AUTOINCREMENT` (9 tables: projects, project_aliases, issues, comments, links, events, api_tokens, purge_log, import_mappings) | `BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY` |
| `INTEGER PRIMARY KEY` (recurrences) | `BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY` (uniform) |
| `TEXT PRIMARY KEY` (meta.key) | `TEXT PRIMARY KEY` (unchanged) |
| `(issue_id, label) PRIMARY KEY` (issue_labels) | `PRIMARY KEY (issue_id, label)` (unchanged) |
| `INTEGER NOT NULL` (FK columns) | `BIGINT NOT NULL` |
| `TEXT NOT NULL DEFAULT '{}'` (metadata) | `TEXT NOT NULL DEFAULT '{}'` (unchanged — keep TEXT for JSONL fidelity) |
| `TEXT NOT NULL DEFAULT '[]'` (template_labels) | `TEXT NOT NULL DEFAULT '[]'` |
| `DATETIME NOT NULL DEFAULT (strftime(…))` | `TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')` |
| `DATETIME` (nullable) | `TEXT` (nullable) |
| `BOOLEAN` (none today, but reserved) | `BOOLEAN` |

**Check-constraint translations:**

| SQLite | Postgres |
|---|---|
| `CHECK (json_valid(x) AND json_type(x) = 'object')` | `CHECK (jsonb_typeof((x)::jsonb) = 'object')` |
| `CHECK (json_valid(x) AND json_type(x) = 'array')` | `CHECK (jsonb_typeof((x)::jsonb) = 'array')` |
| `CHECK (json_valid(payload))` | `CHECK ((payload)::jsonb IS NOT NULL)` |
| `CHECK (length(uid) = 26)` | `CHECK (length(uid) = 26)` (unchanged) |
| `CHECK (length(trim(name)) > 0)` | `CHECK (length(trim(name)) > 0)` (unchanged) |
| `CHECK (name NOT GLOB '*#*')` | `CHECK (name NOT LIKE '%#%')` |
| `CHECK (short_id NOT GLOB '*[^0-9abcdefghjkmnpqrstvwxyz]*')` | `CHECK (short_id !~ '[^0-9abcdefghjkmnpqrstvwxyz]')` (POSIX regex) |
| `CHECK (lower(substr(uid, 27-len, len)) = short_id)` | `CHECK (lower(substr(uid, 27 - length(short_id), length(short_id))) = short_id)` (substr is 1-based in both) |

**Trigger translations.** Each SQLite `RAISE(ABORT, msg)` trigger becomes a PL/pgSQL trigger function returning a row + a `BEFORE INSERT OR UPDATE` trigger:

```sql
CREATE OR REPLACE FUNCTION enforce_links_same_project() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  from_project BIGINT;
  to_project BIGINT;
BEGIN
  -- Mirrors SQLite: both endpoints must belong to NEW.project_id (not just to
  -- each other — a link between two issues in project B with NEW.project_id=A
  -- is also a violation).
  SELECT project_id INTO from_project FROM issues WHERE id = NEW.from_issue_id;
  SELECT project_id INTO to_project FROM issues WHERE id = NEW.to_issue_id;
  IF from_project IS DISTINCT FROM NEW.project_id
     OR to_project IS DISTINCT FROM NEW.project_id THEN
    RAISE EXCEPTION 'cross-project links are not allowed';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER trg_links_same_project_insert
  BEFORE INSERT ON links
  FOR EACH ROW EXECUTE FUNCTION enforce_links_same_project();
CREATE TRIGGER trg_links_same_project_update
  BEFORE UPDATE ON links
  FOR EACH ROW EXECUTE FUNCTION enforce_links_same_project();
```

All six SQLite triggers port to the same shape: a function per invariant, one or two triggers per table. The SQLite inventory is:

- 2 cross-project triggers on `links` (INSERT + UPDATE), one invariant.
- 2 uid-consistency triggers on `links` (INSERT + UPDATE), one invariant.
- 2 uid-immutable triggers (one on `projects`, one on `issues`), one invariant.

Six SQLite triggers total, three distinct invariants. Postgres ports them as three trigger functions and the same six trigger declarations:

- `enforce_links_same_project()` — fires on `links` BEFORE INSERT/UPDATE. Validates both endpoints belong to `NEW.project_id` (not just to each other). Error text matches SQLite's `cross-project links are not allowed`.
- `enforce_links_uid_consistency()` — fires on `links` BEFORE INSERT/UPDATE. Validates that `from_issue_uid` and `to_issue_uid` match the referenced rows' UIDs (the actual SQLite error texts are `from_issue_uid does not match from_issue_id` and `to_issue_uid does not match to_issue_id`; the PL/pgSQL function raises the same two strings).
- `enforce_uid_immutable()` — fires on `projects` BEFORE UPDATE OF uid and on `issues` BEFORE UPDATE OF uid. Rejects `NEW.uid <> OLD.uid` with SQLite's exact error texts (`projects.uid is immutable` and `issues.uid is immutable`). (One function reused across two triggers — one per table. Other tables with `uid` columns — `recurrences`, `project_aliases`, `purge_log`, `events` — don't have immutability triggers in SQLite either; the port faithfully mirrors that.)

### FTS port (the §5.1 surface)

A dedicated `issues_search(issue_id BIGINT PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE, tsv tsvector NOT NULL)` table plus a GIN index on `tsv`. Four PL/pgSQL sync triggers plus the FK's `ON DELETE CASCADE` cover SQLite's five sync points (issue insert/update/delete + comment insert/delete):

```sql
CREATE OR REPLACE FUNCTION rebuild_issue_search(p_issue_id BIGINT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_title TEXT;
  v_body TEXT;
  v_comments TEXT;
BEGIN
  -- Guard: if the parent issue row no longer exists, skip the rebuild. This
  -- protects against the cascade-deletion case: DELETE on issues cascades
  -- through comments first, firing this trigger for each removed comment;
  -- by the time the last comment-delete fires, the issue may already be
  -- vanishing. We don't want to re-insert an issues_search row only to have
  -- it cascade-deleted a moment later, and we definitely don't want a FK
  -- failure on a transient state.
  IF NOT EXISTS (SELECT 1 FROM issues WHERE id = p_issue_id) THEN
    RETURN;
  END IF;
  SELECT title, COALESCE(body, '') INTO v_title, v_body FROM issues WHERE id = p_issue_id;
  -- ORDER BY id matches SQLite's FTS triggers (auto-increment id is the
  -- chronological order kata uses for the comment aggregate).
  SELECT COALESCE(string_agg(body, ' ' ORDER BY id), '')
    INTO v_comments
    FROM comments WHERE issue_id = p_issue_id;
  INSERT INTO issues_search (issue_id, tsv)
    VALUES (p_issue_id,
      to_tsvector('kata_simple_unaccent', coalesce(v_title,'') || ' ' || coalesce(v_body,'') || ' ' || coalesce(v_comments,'')))
    ON CONFLICT (issue_id) DO UPDATE SET tsv = EXCLUDED.tsv;
END $$;
```

Four `AFTER` triggers call this function, matching SQLite's five sync points (the fifth — issue delete — is handled by FK cascade):

- `issues_search_after_issue_insert` — AFTER INSERT ON issues, calls `rebuild_issue_search(NEW.id)`.
- `issues_search_after_issue_update` — AFTER UPDATE OF title, body ON issues, calls `rebuild_issue_search(NEW.id)`.
- *(issue delete)* — the FK `issues_search.issue_id REFERENCES issues(id) ON DELETE CASCADE` removes the search row automatically; no explicit trigger declared. SQLite needs the trigger because FTS5 virtual tables don't honor FK cascades; Postgres does. Sync-point count stays at 5 conceptually but lands as 4 PL/pgSQL triggers + 1 FK CASCADE.
- `issues_search_after_comment_insert` — AFTER INSERT ON comments, calls `rebuild_issue_search(NEW.issue_id)`.
- `issues_search_after_comment_delete` — AFTER DELETE ON comments, calls `rebuild_issue_search(OLD.issue_id)`. SQLite has no comment-UPDATE FTS trigger and the schema has no comment edit path (no `updated_at` or `deleted_at` columns on comments), so the PG side mirrors faithfully — INSERT and DELETE only.

Phase 4 search queries against `tsv @@ websearch_to_tsquery('kata_simple_unaccent', $1)` with `ORDER BY ts_rank_cd(tsv, websearch_to_tsquery('kata_simple_unaccent', $1)) DESC, issues.id ASC` for deterministic ordering. Phase 3 only ships the indexed surface — no query code lives here yet.

### Indexes

Verbatim port of every SQLite index plus the new idempotency-uniqueness pair:

- All `idx_*` partial indexes from `0010_baseline.sql` translate column-for-column. `json_extract(payload, '$.idempotency_key')` becomes `(payload::jsonb ->> 'idempotency_key')`; the partial `WHERE` clause is unchanged in spirit (`type = 'issue.created' AND idempotency_key IS NOT NULL`).
- **New on both backends:**
  - pgstore's 0010 includes `CREATE UNIQUE INDEX idx_events_idempotency_uniq ON events(project_id, (payload::jsonb ->> 'idempotency_key')) WHERE type = 'issue.created' AND (payload::jsonb ->> 'idempotency_key') IS NOT NULL;`.
  - sqlitestore's new `0011_idempotency_unique.sql` adds the equivalent `CREATE UNIQUE INDEX idx_events_idempotency_uniq ON events(project_id, json_extract(payload, '$.idempotency_key')) WHERE type = 'issue.created' AND json_extract(payload, '$.idempotency_key') IS NOT NULL;`.
  - The pre-existing non-unique `idx_events_idempotency` (with `created_at` in the key) remains in place on both backends for the lookup+ordering path.

### `CurrentSchemaVersion` bump

`internal/db/schema_version.go`: `BaselineSchemaVersion` stays at `10` (unchanged — that's the JSONL cutover boundary, immutable). `currentSchemaVersion` advances from `10` to `11`. Existing v10 SQLite DBs auto-apply `0011_idempotency_unique.sql` on the next `Migrate`. Existing tests asserting `CurrentSchemaVersion() == 10` flip to `== 11`.

The migration is forward-compatible: the only failure mode is a contract-violating SQLite DB with pre-existing duplicate idempotency rows, in which case `CREATE UNIQUE INDEX` errors with a clear "duplicate row" message. A short pre-migration check query (`SELECT … GROUP BY … HAVING COUNT(*) > 1`) is documented in the migration file's header comment so operators can verify clean state before upgrading.

### Caller flips (small surface)

- `internal/db/storeopen/storeopen.go`: the existing postgres branch ("not yet available") now imports `internal/db/pgstore` and dispatches. The `jsonlCutoverThreshold` constant doesn't apply to PG (no pre-v10 PG history); the orchestration calls pgstore.Open + store.Migrate directly without the cutover branch.
- `internal/db/storeopen/storeopen_test.go`: a new test `TestOpenPostgresWithApplyMigrationsErrorsForUnreachableHost` confirms the dispatcher reaches pgstore.Open and propagates a real connection error (not the old "not yet available" stub). Real-PG conformance tests defer to Phase 6.
- `cmd/kata/migrate.go`: unchanged. `localSQLitePath` already returns `(_, false)` for `postgres://` DSNs, so the parent-dir creation step correctly skips. The `storeopen.Open` call works unchanged.

### Test strategy

- **Unit-style migration tests** (`pgstore/migrate_test.go`): mirror sqlitestore's migrate tests but use `testing.Short()` to skip the testcontainers-backed cases when running fast. The at-current noop, synthetic 11+, rollback, concurrent, and ladder-validation tests all need a real Postgres because pgx can't run against an in-memory engine.
- **Schema-shape tests** (`pgstore/schema_test.go`): assert every table, every CHECK constraint name, every index name, every trigger name exists with the expected shape. Run against the testcontainers PG instance. These are the Phase 3 acceptance gates — pgstore's Migrate produces a v10 schema indistinguishable in shape from sqlitestore's.
- **sqlitestore 0011 test** (`sqlitestore/migrate_test.go` extension): a new test loads a synthetic v10 DB with at-most-one row per `(project_id, key)` and verifies the 0011 migration applies cleanly. A second test loads a v10 DB with deliberate duplicates and verifies the migration fails with a recognizable error mentioning the duplicate key.
- **Testcontainers infrastructure**: a new `internal/testenv/pgenv.go` helper spins up `postgres:17-alpine` via `github.com/testcontainers/testcontainers-go`, returns the DSN, and registers cleanup. Used by Phase 3's pgstore tests; Phase 4 builds out the full conformance harness on top.

`go test -short ./...` skips the testcontainers cases for fast laptop iteration. CI runs without `-short` and exercises both backends.

## Success criteria

- `internal/db/pgstore/migrations/0010_baseline.sql` produces a v10-shaped Postgres schema (every table, every constraint, every trigger, every index from §5 present and named per the spec). Verified by `schema_test.go` against a fresh testcontainers PG.
- `internal/db/sqlitestore/migrations/0011_idempotency_unique.sql` adds the UNIQUE partial index. Migrating an existing clean v10 SQLite DB succeeds; migrating a duplicate-bearing DB fails loudly with the duplicate key.
- `db.CurrentSchemaVersion() == 11` for both backends.
- `storeopen.Open(ctx, "postgres://…", db.ApplyMigrations())` dispatches to pgstore.Open and calls `store.Migrate`. A fresh PG instance reaches `meta.schema_version = 11` after one round: 0010 stamps to v10, the v11 placeholder file stamps to v11 (see the Phase-3 boundary note below).
- Existing SQLite users see the 0011 migration apply once on next `kata migrate` (or on next daemon start with `ApplyMigrations()`). After that: silent no-op every subsequent open.
- `go test ./...` and `nix run 'nixpkgs#golangci-lint' -- run ./...` both clean.
- `cmd/kata/{migrate,daemon,export,import}` unchanged in behavior — they delegate to `storeopen.Open` and `Migrate`, which now handles both backends.

**Phase-3 boundary on schema_version meaning:**
- pgstore's `0010_baseline.sql` IS the v10 baseline for Postgres (it stamps `meta.schema_version = 10`).
- pgstore needs a 0011-equivalent too, since both backends share `CurrentSchemaVersion()`. Phase 3 ships `pgstore/migrations/0011_idempotency_unique.sql` as a no-op-relative-to-baseline file (the UNIQUE index is already in 0010 for PG since pgstore is fresh ground). The 0011 file body for PG is a single comment + `SELECT 1;` that just gives the runner a version to stamp.
- This keeps the ladder lockstep across backends. If a future migration needs to land on PG only (or SQLite only), that's a future-phase design issue (likely "backend-specific subdirectories" or a router flag); Phase 3 doesn't introduce the divergence.

## Risks

- **Trigger semantics divergence under high concurrency.** Postgres triggers fire per-row inside transactions; SQLite triggers fire per-row inside the implicit single-writer transaction. Multi-statement DML on PG can interleave trigger fires across rows; on SQLite it can't. Mitigation: every trigger is BEFORE-ROW and validates a single invariant. None of the existing six SQLite triggers depends on inter-row ordering, so the port is safe — but Phase 6 conformance tests must exercise multi-row inserts under PG to catch any latent assumption.
- **`ts_rank` ordering instability.** Two issues with identical rank can flip order between identical-shape PG instances if the planner picks different index scan orders. Mitigation per parent spec: queries (Phase 4) always append `id ASC` as a deterministic tiebreaker. Documented but not enforced by Phase 3 schema.
- **Connection pool exhaustion under burst.** `SetMaxOpenConns(25)` is conservative but could throttle a busy daemon. Mitigation: DSN params make it tunable, and Phase 4 will observe in conformance.
- **Extension availability.** `CREATE EXTENSION IF NOT EXISTS unaccent` requires either superuser or `CREATEROLE WITH ADMIN OPTION ON DATABASE` — most managed PG (RDS, Cloud SQL, Azure DB) allow it; self-hosted may need `apt install postgresql-contrib` first. Migration fails loudly with PG's `permission denied to create extension "unaccent"` if missing; the error text alone is enough to point an operator at the install step.
- **Existing SQLite DBs with idempotency duplicates.** If a real-world DB has them (shouldn't, per kata's design, but might if someone manually edited the table), the 0011 migration fails. The recovery path is documented in the migration file header — the operator runs the duplicate-detection query, decides which row to keep, deletes the others, retries `kata migrate`. The snapshot Migrate already took before the failure provides rollback if needed.

## Open questions (for refine)

None of substance. The brainstorming decisions (dedicated issues_search table, simple+unaccent tokenizer, separate UNIQUE partial index on both backends) plus parent-spec §5's pre-pinned translations cover the design surface. /refine should catch wording / consistency issues before the plan is written.
