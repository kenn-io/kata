-- Status sync fields belong to an existing identity mapping. Existing rows
-- receive NULL, so upgrading creates no observation or outbound intent.
ALTER TABLE import_mappings
  ADD COLUMN observed_status TEXT,
  ADD COLUMN observed_status_at TEXT,
  ADD COLUMN pending_event_uid TEXT,
  ADD COLUMN remote_locator TEXT;
