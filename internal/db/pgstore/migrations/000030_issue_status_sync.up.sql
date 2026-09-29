-- Status checkpoints belong to an existing identity mapping. Existing rows
-- intentionally have no observation or outbound intent after upgrade.
ALTER TABLE import_mappings
  ADD COLUMN status_sync_json TEXT DEFAULT NULL
    CHECK (status_sync_json IS NULL OR (status_sync_json IS JSON OBJECT));
