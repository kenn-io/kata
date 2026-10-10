ALTER TABLE comments
  ADD COLUMN reply_to_uid TEXT,
  ADD COLUMN reply_kind TEXT,
  ADD COLUMN edited_at TEXT;
