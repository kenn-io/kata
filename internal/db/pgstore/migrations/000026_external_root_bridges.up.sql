CREATE TABLE external_root_bindings (
  id                       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid                      TEXT NOT NULL UNIQUE,
  project_id               BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  issue_id                 BIGINT NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  root_mapping_id          BIGINT NOT NULL REFERENCES import_mappings(id) ON DELETE CASCADE,
  connector_instance       TEXT NOT NULL,
  external_root_key        TEXT NOT NULL,
  external_account_key     TEXT NOT NULL,
  active                   INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0,1)),
  enabled                  INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1)),
  paused_reason            TEXT NOT NULL DEFAULT '',
  receive_comments         INTEGER NOT NULL DEFAULT 1 CHECK(receive_comments IN (0,1)),
  receive_comments_after   TEXT,
  publish_comments         INTEGER NOT NULL DEFAULT 0 CHECK(publish_comments IN (0,1)),
  publish_comments_after   TEXT,
  complete_external        INTEGER NOT NULL DEFAULT 1 CHECK(complete_external IN (0,1)),
  claim_token              TEXT NOT NULL DEFAULT '',
  claim_started_at         TEXT,
  last_external_state      TEXT NOT NULL DEFAULT '',
  last_external_revision   TEXT NOT NULL DEFAULT '',
  pending_comment_uid      TEXT NOT NULL DEFAULT '',
  pending_comment_started_at TEXT,
  last_attempt_at          TEXT,
  last_success_at          TEXT,
  last_error_at            TEXT,
  last_error               TEXT NOT NULL DEFAULT '',
  consecutive_failures     INTEGER NOT NULL DEFAULT 0 CHECK(consecutive_failures >= 0),
  next_attempt_at          TEXT,
  created_at               TEXT NOT NULL,
  updated_at               TEXT NOT NULL,
  unbound_at               TEXT,
  CHECK(length(trim(connector_instance)) > 0),
  CHECK(length(trim(external_root_key)) > 0),
  CHECK(length(trim(external_account_key)) > 0)
);
CREATE UNIQUE INDEX idx_external_root_bindings_active_issue
  ON external_root_bindings(issue_id) WHERE active = 1;
CREATE UNIQUE INDEX idx_external_root_bindings_active_root
  ON external_root_bindings(connector_instance, external_root_key) WHERE active = 1;
CREATE INDEX idx_external_root_bindings_due
  ON external_root_bindings(active, enabled, last_attempt_at);

CREATE TABLE external_field_mappings (
  id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  connector_instance TEXT NOT NULL,
  kata_field          TEXT NOT NULL CHECK(kata_field IN ('scheduled_on','deadline_on')),
  external_field_id   TEXT NOT NULL,
  external_field_name TEXT NOT NULL,
  accepted_kinds_json TEXT NOT NULL CHECK(accepted_kinds_json IS JSON),
  nullable            INTEGER NOT NULL CHECK(nullable IN (0,1)),
  writable            INTEGER NOT NULL CHECK(writable IN (0,1)),
  schema_revision     TEXT NOT NULL,
  active              INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0,1)),
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_external_field_mappings_active
  ON external_field_mappings(connector_instance, kata_field) WHERE active = 1;

CREATE TABLE external_field_states (
  binding_id        BIGINT NOT NULL REFERENCES external_root_bindings(id) ON DELETE CASCADE,
  mapping_id        BIGINT NOT NULL REFERENCES external_field_mappings(id),
  baseline_json     TEXT CHECK(baseline_json IS NULL OR baseline_json IS JSON),
  conflicted        INTEGER NOT NULL DEFAULT 0 CHECK(conflicted IN (0,1)),
  conflict_kata     TEXT CHECK(conflict_kata IS NULL OR conflict_kata IS JSON),
  conflict_external TEXT CHECK(conflict_external IS NULL OR conflict_external IS JSON),
  conflict_at       TEXT,
  updated_at        TEXT NOT NULL,
  PRIMARY KEY(binding_id, mapping_id)
);
