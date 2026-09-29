-- Replace the private checkpoint object with nullable mapping fields. A null
-- raw status remains an observation when its provider timestamp is present.
ALTER TABLE import_mappings
  ADD COLUMN observed_status TEXT,
  ADD COLUMN observed_status_at TEXT,
  ADD COLUMN pending_event_uid TEXT,
  ADD COLUMN remote_locator TEXT;

UPDATE import_mappings
   SET observed_status = status_sync_json::jsonb #>> '{observed,raw}',
       observed_status_at = status_sync_json::jsonb #>> '{observed,version}',
       pending_event_uid = status_sync_json::jsonb ->> 'pending_event_uid',
       remote_locator = status_sync_json::jsonb ->> 'github_issue_number'
 WHERE status_sync_json IS NOT NULL;

ALTER TABLE import_mappings DROP COLUMN status_sync_json;
