-- Canonical kata Postgres schema at db.CurrentSchemaVersion(). Semantically
-- equivalent to internal/db/sqlitestore/schema.sql, with the SQLite-specific
-- constructs translated to Postgres below.
--
-- Translation conventions:
--   INTEGER PRIMARY KEY AUTOINCREMENT  -> BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY
--   INTEGER NOT NULL (FK)              -> BIGINT NOT NULL
--   DATETIME default strftime          -> TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', ...)
--   DATETIME nullable                  -> TEXT nullable
--   json_valid(x) AND json_type(x)='X' -> jsonb_typeof((x)::jsonb) = 'X'
--   json_valid(x)                      -> (x)::jsonb IS NOT NULL
--   x NOT GLOB '*pat*'                 -> x NOT LIKE '%pat%'
--   x NOT GLOB '*[^...]*'              -> x !~ '[^...]'                     (POSIX regex)
--   json_extract(p, '$.k')             -> (p::jsonb ->> 'k')
--   FTS5 virtual table + 5 triggers    -> issues_search table + 4 triggers + FK CASCADE
--   SQLite RAISE(ABORT, msg) trigger   -> PL/pgSQL function + trigger
--
-- Table order is FK-dependency: parents before children. recurrences appears
-- before issues because issues references recurrences.

CREATE EXTENSION IF NOT EXISTS unaccent;

-- Custom text-search config: unaccent over simple. Same lower-no-stem
-- tokenization as SQLite's `unicode61 remove_diacritics 2`.
DROP TEXT SEARCH CONFIGURATION IF EXISTS kata_simple_unaccent;
CREATE TEXT SEARCH CONFIGURATION kata_simple_unaccent (COPY = simple);
ALTER TEXT SEARCH CONFIGURATION kata_simple_unaccent
  ALTER MAPPING FOR hword, hword_part, word
  WITH unaccent, simple;

-- ----------------------------------------------------------------------
-- meta: schema_version / instance_uid / created_by_version (key/value).
-- ----------------------------------------------------------------------
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT INTO meta(key, value) VALUES ('created_by_version', '0.1.0');

-- ----------------------------------------------------------------------
-- projects: top-level project table referenced by everything else.
-- ----------------------------------------------------------------------
CREATE TABLE projects (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid        TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  deleted_at TEXT,
  metadata   TEXT NOT NULL DEFAULT '{}'
               CHECK (jsonb_typeof((metadata)::jsonb) = 'object'),
  revision   BIGINT NOT NULL DEFAULT 1,
  CHECK (length(uid) = 26),
  CHECK (length(trim(name)) > 0),
  CHECK (name NOT LIKE '%#%')
);
CREATE INDEX idx_projects_active ON projects(id) WHERE deleted_at IS NULL;

CREATE TABLE project_aliases (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id      BIGINT NOT NULL REFERENCES projects(id),
  alias_identity  TEXT UNIQUE NOT NULL,
  alias_kind      TEXT NOT NULL CHECK(alias_kind IN ('git','local')),
  root_path       TEXT NOT NULL,
  created_at      TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  last_seen_at    TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  CHECK (length(trim(alias_identity)) > 0),
  CHECK (length(trim(root_path)) > 0)
);
CREATE INDEX idx_project_aliases_project ON project_aliases(project_id);

-- recurrences before issues: issues.recurrence_id references recurrences(id).
CREATE TABLE recurrences (
  id                     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid                    TEXT NOT NULL UNIQUE CHECK (length(uid) = 26),
  project_id             BIGINT NOT NULL
                           REFERENCES projects(id) ON DELETE CASCADE,
  rrule                  TEXT NOT NULL,
  dtstart                TEXT NOT NULL,
  timezone               TEXT NOT NULL,
  template_title         TEXT NOT NULL,
  template_body          TEXT NOT NULL DEFAULT '',
  template_owner         TEXT,
  template_priority      INTEGER CHECK (template_priority IS NULL
                            OR template_priority BETWEEN 0 AND 4),
  template_labels        TEXT NOT NULL DEFAULT '[]'
                           CHECK (jsonb_typeof((template_labels)::jsonb) = 'array'),
  template_metadata      TEXT NOT NULL DEFAULT '{}'
                           CHECK (jsonb_typeof((template_metadata)::jsonb) = 'object'),
  next_occurrence_key    TEXT,
  last_materialized_uid  TEXT,
  author                 TEXT NOT NULL CHECK (length(trim(author)) > 0),
  revision               BIGINT NOT NULL DEFAULT 1,
  created_at             TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  updated_at             TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  deleted_at             TEXT
);
CREATE INDEX recurrences_project ON recurrences(project_id)
  WHERE deleted_at IS NULL;

CREATE TABLE issues (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid           TEXT NOT NULL UNIQUE,
  project_id    BIGINT NOT NULL REFERENCES projects(id),
  short_id      TEXT NOT NULL,
  title         TEXT NOT NULL,
  body          TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL CHECK(status IN ('open','closed')) DEFAULT 'open',
  closed_reason TEXT CHECK(closed_reason IN ('done','wontfix','duplicate','superseded','audit-no-change')),
  owner         TEXT,
  priority      INTEGER,
  author        TEXT NOT NULL,
  created_at    TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  updated_at    TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  closed_at     TEXT,
  deleted_at    TEXT,
  metadata      TEXT NOT NULL DEFAULT '{}'
                  CHECK (jsonb_typeof((metadata)::jsonb) = 'object'),
  revision      BIGINT NOT NULL DEFAULT 1,
  recurrence_id   BIGINT REFERENCES recurrences(id) ON DELETE SET NULL,
  occurrence_key  TEXT,
  CHECK (length(uid) = 26),
  CHECK (length(trim(title))  > 0),
  CHECK (length(trim(author)) > 0),
  CHECK (status = 'closed' OR (closed_at IS NULL AND closed_reason IS NULL)),
  CHECK (priority IS NULL OR priority BETWEEN 0 AND 4),
  CHECK (length(short_id) BETWEEN 4 AND 26),
  CHECK (short_id !~ '[^0-9abcdefghjkmnpqrstvwxyz]'),
  CHECK (short_id = lower(substr(uid, 27 - length(short_id), length(short_id))))
);
CREATE INDEX idx_issues_project_status_updated
  ON issues(project_id, status, updated_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_issues_project_updated
  ON issues(project_id, updated_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_issues_owner
  ON issues(owner) WHERE owner IS NOT NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX uniq_issues_project_short_id
  ON issues(project_id, short_id);
CREATE UNIQUE INDEX issues_recurrence_occurrence_uniq
  ON issues(recurrence_id, occurrence_key)
  WHERE recurrence_id IS NOT NULL AND occurrence_key IS NOT NULL;

CREATE TABLE comments (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid        TEXT NOT NULL UNIQUE,
  issue_id   BIGINT NOT NULL REFERENCES issues(id),
  author     TEXT NOT NULL,
  body       TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  CHECK (length(uid) = 26),
  CHECK (length(trim(author)) > 0),
  CHECK (length(trim(body))   > 0)
);
CREATE INDEX idx_comments_issue ON comments(issue_id, created_at);

CREATE TABLE links (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id    BIGINT NOT NULL REFERENCES projects(id),
  from_issue_id BIGINT NOT NULL REFERENCES issues(id),
  to_issue_id   BIGINT NOT NULL REFERENCES issues(id),
  from_issue_uid TEXT NOT NULL,
  to_issue_uid   TEXT NOT NULL,
  type          TEXT NOT NULL CHECK(type IN ('parent','blocks','related')),
  author        TEXT NOT NULL,
  created_at    TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
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

-- ----------------------------------------------------------------------
-- Trigger functions: PG ports of the six SQLite RAISE(ABORT, ...) triggers.
-- ----------------------------------------------------------------------

-- enforce_links_same_project: both endpoints must belong to NEW.project_id.
CREATE OR REPLACE FUNCTION enforce_links_same_project() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  from_project BIGINT;
  to_project BIGINT;
BEGIN
  SELECT project_id INTO from_project FROM issues WHERE id = NEW.from_issue_id;
  SELECT project_id INTO to_project   FROM issues WHERE id = NEW.to_issue_id;
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

-- enforce_links_uid_consistency: stored UIDs must match the referenced rows.
CREATE OR REPLACE FUNCTION enforce_links_uid_consistency() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  from_uid TEXT;
  to_uid TEXT;
BEGIN
  SELECT uid INTO from_uid FROM issues WHERE id = NEW.from_issue_id;
  SELECT uid INTO to_uid   FROM issues WHERE id = NEW.to_issue_id;
  IF NEW.from_issue_uid IS DISTINCT FROM from_uid THEN
    RAISE EXCEPTION 'from_issue_uid does not match from_issue_id';
  END IF;
  IF NEW.to_issue_uid IS DISTINCT FROM to_uid THEN
    RAISE EXCEPTION 'to_issue_uid does not match to_issue_id';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER trg_links_uid_consistency_insert
  BEFORE INSERT ON links
  FOR EACH ROW EXECUTE FUNCTION enforce_links_uid_consistency();
CREATE TRIGGER trg_links_uid_consistency_update
  BEFORE UPDATE ON links
  FOR EACH ROW EXECUTE FUNCTION enforce_links_uid_consistency();

-- enforce_uid_immutable: projects.uid and issues.uid are write-once.
CREATE OR REPLACE FUNCTION enforce_uid_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.uid IS DISTINCT FROM OLD.uid THEN
    RAISE EXCEPTION '%.uid is immutable', TG_TABLE_NAME;
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER trg_projects_uid_immutable
  BEFORE UPDATE OF uid ON projects
  FOR EACH ROW EXECUTE FUNCTION enforce_uid_immutable();
CREATE TRIGGER trg_issues_uid_immutable
  BEFORE UPDATE OF uid ON issues
  FOR EACH ROW EXECUTE FUNCTION enforce_uid_immutable();

CREATE TABLE issue_labels (
  issue_id   BIGINT NOT NULL REFERENCES issues(id),
  label      TEXT NOT NULL,
  author     TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  PRIMARY KEY(issue_id, label),
  CHECK (length(label) BETWEEN 1 AND 64),
  CHECK (label !~ '[^a-z0-9._:-]'),
  CHECK (length(trim(author)) > 0)
);
CREATE INDEX idx_issue_labels_label ON issue_labels(label);

CREATE TABLE events (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid                 TEXT NOT NULL UNIQUE,
  origin_instance_uid TEXT NOT NULL,
  project_id          BIGINT NOT NULL REFERENCES projects(id),
  project_name        TEXT NOT NULL,
  issue_id            BIGINT REFERENCES issues(id),
  issue_uid           TEXT,
  related_issue_id    BIGINT REFERENCES issues(id),
  related_issue_uid   TEXT,
  type                TEXT NOT NULL,
  actor               TEXT NOT NULL,
  payload             TEXT NOT NULL DEFAULT '{}',
  hlc_physical_ms     BIGINT NOT NULL,
  hlc_counter         BIGINT NOT NULL,
  content_hash        TEXT NOT NULL,
  created_at          TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  CHECK (length(trim(actor)) > 0),
  CHECK ((payload)::jsonb IS NOT NULL),
  CHECK (length(uid) = 26),
  CHECK (length(origin_instance_uid) = 26),
  CHECK (hlc_physical_ms > 0),
  CHECK (hlc_counter >= 0),
  CHECK (length(content_hash) = 64)
);
CREATE INDEX idx_events_project ON events(project_id, id);
CREATE INDEX idx_events_issue   ON events(issue_id, id) WHERE issue_id IS NOT NULL;
CREATE INDEX idx_events_related ON events(related_issue_id, id) WHERE related_issue_id IS NOT NULL;
CREATE INDEX idx_events_issue_uid ON events(issue_uid) WHERE issue_uid IS NOT NULL;
CREATE INDEX idx_events_related_issue_uid ON events(related_issue_uid) WHERE related_issue_uid IS NOT NULL;
CREATE INDEX idx_events_origin_instance ON events(origin_instance_uid);
CREATE INDEX idx_events_origin_project_id
  ON events(origin_instance_uid, project_id, id);
CREATE INDEX idx_events_hlc ON events(hlc_physical_ms, hlc_counter, origin_instance_uid, uid);
CREATE INDEX idx_events_content_hash ON events(content_hash);
CREATE INDEX idx_events_idempotency
  ON events(project_id, (payload::jsonb ->> 'idempotency_key'), created_at)
  WHERE type = 'issue.created' AND (payload::jsonb ->> 'idempotency_key') IS NOT NULL;

CREATE TABLE api_tokens (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  token_hash   TEXT NOT NULL UNIQUE,
  actor        TEXT NOT NULL,
  name         TEXT,
  created_at   TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  last_used_at TEXT,
  revoked_at   TEXT,
  CHECK (length(token_hash) = 64),
  CHECK (length(trim(actor)) > 0),
  CHECK (actor <> 'bootstrap'),
  CHECK (name IS NULL OR length(trim(name)) > 0)
);

CREATE TABLE purge_log (
  id                          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid                         TEXT NOT NULL UNIQUE,
  origin_instance_uid         TEXT NOT NULL,
  project_id                  BIGINT NOT NULL,
  purged_issue_id             BIGINT NOT NULL,
  issue_uid                   TEXT,
  project_uid                 TEXT,
  project_name                TEXT NOT NULL,
  issue_title                 TEXT NOT NULL,
  issue_author                TEXT NOT NULL,
  comment_count               BIGINT NOT NULL,
  link_count                  BIGINT NOT NULL,
  label_count                 BIGINT NOT NULL,
  event_count                 BIGINT NOT NULL,
  events_deleted_min_id       BIGINT,
  events_deleted_max_id       BIGINT,
  purge_reset_after_event_id  BIGINT,
  short_id                    TEXT,
  actor                       TEXT NOT NULL,
  reason                      TEXT,
  purged_at                   TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  CHECK (length(trim(actor)) > 0),
  CHECK (length(uid) = 26),
  CHECK (length(origin_instance_uid) = 26),
  CHECK (
    short_id IS NULL OR (
      length(short_id) BETWEEN 4 AND 26
      AND short_id !~ '[^0-9abcdefghjkmnpqrstvwxyz]'
    )
  )
);
CREATE INDEX idx_purge_log_reset
  ON purge_log(purge_reset_after_event_id) WHERE purge_reset_after_event_id IS NOT NULL;
CREATE INDEX idx_purge_log_project_reset
  ON purge_log(project_id, purge_reset_after_event_id) WHERE purge_reset_after_event_id IS NOT NULL;
CREATE INDEX idx_purge_log_issue  ON purge_log(purged_issue_id);
CREATE INDEX idx_purge_log_issue_uid ON purge_log(issue_uid) WHERE issue_uid IS NOT NULL;
CREATE INDEX idx_purge_log_project_uid ON purge_log(project_uid) WHERE project_uid IS NOT NULL;
CREATE INDEX idx_purge_log_origin_instance ON purge_log(origin_instance_uid);
CREATE INDEX idx_purge_log_short_id
  ON purge_log(project_id, short_id) WHERE short_id IS NOT NULL;

-- ----------------------------------------------------------------------
-- Federation tables.
-- ----------------------------------------------------------------------

CREATE TABLE federation_bindings (
  project_id              BIGINT PRIMARY KEY REFERENCES projects(id),
  role                    TEXT NOT NULL CHECK(role IN ('hub','spoke')),
  hub_url                 TEXT NOT NULL DEFAULT '',
  hub_project_id          BIGINT NOT NULL DEFAULT 0,
  hub_project_uid         TEXT NOT NULL,
  replay_horizon_event_id BIGINT NOT NULL DEFAULT 0,
  pull_cursor_event_id    BIGINT NOT NULL DEFAULT 0,
  push_enabled            INTEGER NOT NULL DEFAULT 0 CHECK(push_enabled IN (0,1)),
  push_cursor_event_id    BIGINT NOT NULL DEFAULT 0 CHECK(push_cursor_event_id >= 0),
  enabled                 INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
  created_at              TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  updated_at              TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  last_sync_at            TEXT,
  CHECK (length(hub_project_uid) = 26),
  CHECK (role = 'hub' OR length(trim(hub_url)) > 0),
  CHECK (role = 'hub' OR hub_project_id > 0),
  CHECK (replay_horizon_event_id >= 0),
  CHECK (pull_cursor_event_id >= 0)
);
CREATE INDEX idx_federation_bindings_role_enabled
  ON federation_bindings(role, enabled);

CREATE TABLE federation_sync_status (
  project_id              BIGINT PRIMARY KEY REFERENCES projects(id),
  last_pull_started_at    TEXT,
  last_pull_success_at    TEXT,
  last_push_started_at    TEXT,
  last_push_success_at    TEXT,
  last_error_at           TEXT,
  last_error              TEXT,
  last_reset_at           TEXT
);

CREATE TABLE federation_quarantine (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id     BIGINT NOT NULL REFERENCES projects(id),
  direction      TEXT NOT NULL CHECK(direction IN ('push','pull')),
  first_event_id BIGINT NOT NULL CHECK(first_event_id >= 0),
  last_event_id  BIGINT NOT NULL CHECK(last_event_id >= first_event_id),
  event_uids     TEXT NOT NULL DEFAULT '[]' CHECK((event_uids)::jsonb IS NOT NULL),
  error          TEXT NOT NULL,
  created_at     TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  skipped_at     TEXT,
  skipped_by     TEXT,
  skip_reason    TEXT,
  CHECK (length(trim(error)) > 0),
  CHECK (skipped_at IS NULL OR length(trim(skipped_by)) > 0)
);
CREATE UNIQUE INDEX uniq_federation_quarantine_active
  ON federation_quarantine(project_id, direction)
  WHERE skipped_at IS NULL;

CREATE TABLE federation_enrollments (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  token_hash          TEXT NOT NULL UNIQUE,
  spoke_instance_uid  TEXT NOT NULL,
  project_id          BIGINT REFERENCES projects(id),
  capabilities        TEXT NOT NULL,
  created_at          TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  updated_at          TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  revoked_at          TEXT,
  CHECK (length(token_hash) = 64),
  CHECK (length(spoke_instance_uid) = 26),
  CHECK (length(trim(capabilities)) > 0)
);
CREATE INDEX idx_federation_enrollments_scope
  ON federation_enrollments(project_id, revoked_at);
CREATE INDEX idx_federation_enrollments_spoke
  ON federation_enrollments(spoke_instance_uid);

CREATE TABLE issue_claims (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  claim_uid           TEXT NOT NULL UNIQUE,
  project_id          BIGINT NOT NULL REFERENCES projects(id),
  issue_id            BIGINT NOT NULL REFERENCES issues(id),
  issue_uid           TEXT NOT NULL,
  holder              TEXT NOT NULL,
  holder_instance_uid TEXT NOT NULL,
  client_kind         TEXT NOT NULL DEFAULT '',
  purpose             TEXT NOT NULL DEFAULT '',
  claim_kind          TEXT NOT NULL CHECK(claim_kind IN ('hard','timed')),
  acquired_at         TEXT NOT NULL,
  expires_at          TEXT,
  released_at         TEXT,
  release_reason      TEXT,
  revision            BIGINT NOT NULL DEFAULT 1,
  updated_at          TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  CHECK (length(claim_uid) = 26),
  CHECK (length(issue_uid) = 26),
  CHECK (length(holder_instance_uid) = 26),
  CHECK (length(trim(holder)) > 0),
  CHECK (claim_kind = 'hard' OR expires_at IS NOT NULL),
  CHECK (claim_kind = 'timed' OR expires_at IS NULL)
);
CREATE UNIQUE INDEX uniq_issue_claims_live_issue
  ON issue_claims(issue_uid)
  WHERE released_at IS NULL;
CREATE INDEX idx_issue_claims_project_issue
  ON issue_claims(project_id, issue_id, released_at);
CREATE INDEX idx_issue_claims_timed_expiry
  ON issue_claims(expires_at)
  WHERE released_at IS NULL AND claim_kind = 'timed';

CREATE TABLE pending_claim_requests (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  request_uid     TEXT NOT NULL UNIQUE,
  project_id      BIGINT NOT NULL REFERENCES projects(id),
  issue_id        BIGINT NOT NULL REFERENCES issues(id),
  issue_uid       TEXT NOT NULL,
  holder          TEXT NOT NULL,
  holder_instance_uid TEXT NOT NULL DEFAULT '',
  client_kind     TEXT NOT NULL DEFAULT '',
  claim_kind      TEXT NOT NULL CHECK(claim_kind IN ('hard','timed')),
  ttl_seconds     INTEGER,
  purpose         TEXT NOT NULL DEFAULT '',
  requested_at    TEXT NOT NULL,
  last_attempt_at TEXT,
  last_error      TEXT,
  rejected_at     TEXT,
  resolved_at     TEXT,
  CHECK (length(request_uid) = 26),
  CHECK (length(issue_uid) = 26),
  CHECK (length(trim(holder)) > 0)
);
CREATE UNIQUE INDEX uniq_pending_claim_active
  ON pending_claim_requests(issue_uid, holder_instance_uid, holder, client_kind)
  WHERE rejected_at IS NULL AND resolved_at IS NULL;

-- ----------------------------------------------------------------------
-- FTS surface: issues_search table + rebuild function + 4 sync triggers.
-- FK CASCADE replaces SQLite's issue-delete FTS trigger.
-- ----------------------------------------------------------------------

CREATE TABLE issues_search (
  issue_id BIGINT PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
  tsv      tsvector NOT NULL
);
CREATE INDEX idx_issues_search_tsv ON issues_search USING GIN(tsv);

CREATE OR REPLACE FUNCTION rebuild_issue_search(p_issue_id BIGINT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_title TEXT;
  v_body TEXT;
  v_comments TEXT;
BEGIN
  -- Cascade guard: when the parent issue row is already vanishing the
  -- rebuild must skip. DELETE on issues cascades through comments first,
  -- firing comment-delete triggers; by the time the last one fires the
  -- issue row may already be gone, and re-inserting a search row would
  -- only be cascade-deleted moments later.
  IF NOT EXISTS (SELECT 1 FROM issues WHERE id = p_issue_id) THEN
    RETURN;
  END IF;
  SELECT title, COALESCE(body, '') INTO v_title, v_body FROM issues WHERE id = p_issue_id;
  SELECT COALESCE(string_agg(body, ' ' ORDER BY id), '')
    INTO v_comments
    FROM comments WHERE issue_id = p_issue_id;
  INSERT INTO issues_search (issue_id, tsv)
    VALUES (p_issue_id,
      to_tsvector('kata_simple_unaccent',
        coalesce(v_title,'') || ' ' || coalesce(v_body,'') || ' ' || coalesce(v_comments,'')))
    ON CONFLICT (issue_id) DO UPDATE SET tsv = EXCLUDED.tsv;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_issue() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(NEW.id);
  RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_comment_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(NEW.issue_id);
  RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_comment_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(OLD.issue_id);
  RETURN NULL;
END $$;

CREATE TRIGGER issues_search_after_issue_insert
  AFTER INSERT ON issues
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_issue();

CREATE TRIGGER issues_search_after_issue_update
  AFTER UPDATE OF title, body ON issues
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_issue();

CREATE TRIGGER issues_search_after_comment_insert
  AFTER INSERT ON comments
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_comment_insert();

CREATE TRIGGER issues_search_after_comment_delete
  AFTER DELETE ON comments
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_comment_delete();

-- ----------------------------------------------------------------------
-- import_mappings: cross-source ID map, last (no FK targets above it).
-- ----------------------------------------------------------------------
CREATE TABLE import_mappings (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source            TEXT NOT NULL,
  external_id       TEXT NOT NULL,
  object_type       TEXT NOT NULL CHECK(object_type IN ('issue','comment','label','link')),
  project_id        BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  issue_id          BIGINT REFERENCES issues(id) ON DELETE CASCADE,
  comment_id        BIGINT REFERENCES comments(id) ON DELETE CASCADE,
  link_id           BIGINT REFERENCES links(id) ON DELETE CASCADE,
  label             TEXT,
  source_updated_at TEXT,
  imported_at       TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  UNIQUE(source, external_id, object_type, project_id),
  CHECK (length(trim(source)) > 0),
  CHECK (length(trim(external_id)) > 0),
  CHECK (object_type != 'issue' OR issue_id IS NOT NULL),
  CHECK (object_type != 'comment' OR (issue_id IS NOT NULL AND comment_id IS NOT NULL)),
  CHECK (object_type != 'label' OR (issue_id IS NOT NULL AND label IS NOT NULL)),
  CHECK (object_type != 'link' OR (issue_id IS NOT NULL AND link_id IS NOT NULL))
);
CREATE INDEX idx_import_mappings_issue ON import_mappings(issue_id);
CREATE INDEX idx_import_mappings_comment ON import_mappings(comment_id);
CREATE INDEX idx_import_mappings_link ON import_mappings(link_id);
