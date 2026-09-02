CREATE TABLE close_event_deliveries (
  project_id       BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  idempotency_key  TEXT NOT NULL,
  issue_uid        TEXT NOT NULL CHECK (length(issue_uid) = 26),
  fingerprint      TEXT NOT NULL CHECK (length(fingerprint) = 64),
  event_uids       TEXT NOT NULL
                     CHECK (jsonb_typeof(event_uids::jsonb) = 'array'
                            AND jsonb_array_length(event_uids::jsonb) > 0),
  state             TEXT NOT NULL DEFAULT 'pending'
                      CHECK (state IN ('pending', 'delivering', 'delivered')),
  claim_token       TEXT,
  claim_expires_at  TEXT,
  delivered_at      TEXT,
  created_at        TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  updated_at        TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  PRIMARY KEY(project_id, idempotency_key),
  CHECK (
    (state = 'pending' AND claim_token IS NULL AND claim_expires_at IS NULL AND delivered_at IS NULL)
    OR (state = 'delivering' AND claim_token IS NOT NULL AND length(trim(claim_token)) > 0 AND claim_expires_at IS NOT NULL AND delivered_at IS NULL)
    OR (state = 'delivered' AND claim_token IS NULL AND claim_expires_at IS NULL AND delivered_at IS NOT NULL)
  )
);
