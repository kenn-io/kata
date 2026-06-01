-- 0013_idempotency_unique.sql: close the check-then-insert race on
-- events(project_id, idempotency_key) for issue.created events. SQLite's
-- single-writer model has historically masked the race in practice; this
-- migration makes the constraint explicit so the daemon's idempotency lookup
-- can rely on it, and so Postgres (which has no single-writer guarantee) can
-- enforce identical semantics out of the box.
--
-- Pre-migration check: a DB with duplicate (project_id, idempotency_key) rows
-- under type='issue.created' will fail CREATE UNIQUE INDEX. Operators can
-- detect duplicates ahead of time with:
--
--   SELECT project_id,
--          json_extract(payload, '$.idempotency_key') AS k,
--          COUNT(*) AS n
--     FROM events
--    WHERE type = 'issue.created'
--      AND json_extract(payload, '$.idempotency_key') IS NOT NULL
--    GROUP BY project_id, k
--   HAVING COUNT(*) > 1;
--
-- and resolve them (keep the earliest, delete the rest) before re-running
-- kata migrate.

CREATE UNIQUE INDEX idx_events_idempotency_uniq
  ON events(project_id, json_extract(payload, '$.idempotency_key'))
  WHERE type = 'issue.created'
    AND json_extract(payload, '$.idempotency_key') IS NOT NULL;
