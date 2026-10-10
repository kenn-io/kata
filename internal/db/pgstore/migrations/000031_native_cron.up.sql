-- Schema 30 to 31: dormant shared cron definitions and attributed run observations.
CREATE TABLE cron_jobs (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid TEXT NOT NULL UNIQUE CHECK (uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  name TEXT NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 256),
  definition_json TEXT NOT NULL CHECK (COALESCE((octet_length(definition_json) <= 262144 AND jsonb_typeof(definition_json::jsonb) = 'object' AND (definition_json::jsonb -> 'version')::text = '1'), FALSE)),
  definition_event_uid TEXT NOT NULL CHECK (definition_event_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  definition_hlc_json TEXT NOT NULL CHECK (COALESCE((octet_length(definition_hlc_json) <= 16384 AND jsonb_typeof(definition_hlc_json::jsonb) = 'object' AND (definition_hlc_json::jsonb -> 'version')::text = '1' AND ((definition_hlc_json::jsonb->'physical_ms')::text ~ '^[0-9]+$') AND ((definition_hlc_json::jsonb->'counter')::text ~ '^[0-9]+$') AND (definition_hlc_json::jsonb->>'physical_ms')::bigint >= 0 AND (definition_hlc_json::jsonb->>'counter')::bigint >= 0), FALSE)),
  author TEXT NOT NULL CHECK (length(trim(author)) > 0),
  revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  updated_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  deleted_at TEXT
);
CREATE INDEX idx_cron_jobs_project_name ON cron_jobs(project_id,name) WHERE deleted_at IS NULL;

CREATE TABLE cron_workflows (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid TEXT NOT NULL UNIQUE CHECK (uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  name TEXT NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 256),
  definition_json TEXT NOT NULL CHECK (COALESCE((octet_length(definition_json) <= 262144 AND jsonb_typeof(definition_json::jsonb) = 'object' AND (definition_json::jsonb -> 'version')::text = '1'), FALSE)),
  definition_event_uid TEXT NOT NULL CHECK (definition_event_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  definition_hlc_json TEXT NOT NULL CHECK (COALESCE((octet_length(definition_hlc_json) <= 16384 AND jsonb_typeof(definition_hlc_json::jsonb) = 'object' AND (definition_hlc_json::jsonb -> 'version')::text = '1' AND ((definition_hlc_json::jsonb->'physical_ms')::text ~ '^[0-9]+$') AND ((definition_hlc_json::jsonb->'counter')::text ~ '^[0-9]+$') AND (definition_hlc_json::jsonb->>'physical_ms')::bigint >= 0 AND (definition_hlc_json::jsonb->>'counter')::bigint >= 0), FALSE)),
  author TEXT NOT NULL CHECK (length(trim(author)) > 0),
  revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  updated_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  deleted_at TEXT
);
CREATE INDEX idx_cron_workflows_project_name ON cron_workflows(project_id,name) WHERE deleted_at IS NULL;

CREATE TABLE cron_runs (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid TEXT NOT NULL UNIQUE CHECK (uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$'),
  project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  job_uid TEXT CHECK (job_uid IS NULL OR (job_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$')),
  definition_event_uid TEXT CHECK (definition_event_uid IS NULL OR (definition_event_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$')),
  workflow_uid TEXT CHECK (workflow_uid IS NULL OR (workflow_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$')),
  workflow_definition_event_uid TEXT CHECK (workflow_definition_event_uid IS NULL OR (workflow_definition_event_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$')),
  occurrence_key TEXT CHECK (occurrence_key IS NULL OR length(occurrence_key) BETWEEN 1 AND 1024),
  issue_uid TEXT CHECK (issue_uid IS NULL OR (issue_uid ~ '^[0-7][0-9A-HJKMNP-TV-Z]{25}$')),
  actor TEXT NOT NULL CHECK (length(trim(actor)) > 0 AND octet_length(actor) <= 256),
  teammate TEXT CHECK (teammate IS NULL OR (length(trim(teammate)) > 0 AND octet_length(teammate) <= 256)),
  executor_label TEXT CHECK (executor_label IS NULL OR (length(trim(executor_label)) > 0 AND octet_length(executor_label) <= 256)),
  status TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','cancelled','unknown')),
  summary_json TEXT NOT NULL DEFAULT '{"version":1}' CHECK (COALESCE((octet_length(summary_json) <= 65536 AND jsonb_typeof(summary_json::jsonb) = 'object' AND (summary_json::jsonb -> 'version')::text = '1' AND (summary_json::jsonb->'input_tokens' IS NULL OR ((summary_json::jsonb->'input_tokens')::text ~ '^[0-9]+$')) AND (summary_json::jsonb->'output_tokens' IS NULL OR ((summary_json::jsonb->'output_tokens')::text ~ '^[0-9]+$'))), FALSE)),
  revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"000Z"') CHECK (created_at ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{9}Z$'),
  started_at TEXT,
  ended_at TEXT,
  updated_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  CHECK ((job_uid IS NULL) = (definition_event_uid IS NULL)),
  CHECK ((workflow_uid IS NULL) = (workflow_definition_event_uid IS NULL)),
  CHECK (job_uid IS NOT NULL OR workflow_uid IS NOT NULL)
);
CREATE INDEX idx_cron_runs_project_time ON cron_runs(project_id,created_at,uid);
CREATE INDEX idx_cron_runs_job_time ON cron_runs(project_id,job_uid,created_at,uid);
CREATE INDEX idx_events_project_cron ON events(project_id) WHERE type LIKE 'cron.%';
