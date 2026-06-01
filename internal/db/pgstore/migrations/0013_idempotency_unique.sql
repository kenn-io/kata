-- 0013_idempotency_unique.sql: Postgres companion to sqlitestore's same-name
-- migration. Closes the check-then-insert race on
-- events(project_id, idempotency_key) for issue.created events.
--
-- On a fresh pgstore database the baseline (0012) already declared the
-- non-unique idx_events_idempotency partial index for the lookup+ordering
-- path; this migration adds the UNIQUE counterpart whose key omits
-- created_at so a duplicate insert raises a constraint violation. Phase 4
-- query code will rely on the UNIQUE index for ON CONFLICT-style idempotency
-- handling.
--
-- Pre-migration check (matches the sqlitestore header):
--
--   SELECT project_id,
--          payload::jsonb ->> 'idempotency_key' AS k,
--          COUNT(*) AS n
--     FROM events
--    WHERE type = 'issue.created'
--      AND (payload::jsonb ->> 'idempotency_key') IS NOT NULL
--    GROUP BY project_id, k
--   HAVING COUNT(*) > 1;

CREATE UNIQUE INDEX idx_events_idempotency_uniq
  ON events(project_id, (payload::jsonb ->> 'idempotency_key'))
  WHERE type = 'issue.created'
    AND (payload::jsonb ->> 'idempotency_key') IS NOT NULL;
