ALTER TABLE comments
  ADD COLUMN reply_to_uid TEXT,
  ADD COLUMN reply_kind TEXT,
  ADD COLUMN edited_at TEXT,
  ADD CONSTRAINT comments_reply_pair CHECK ((reply_to_uid IS NULL) = (reply_kind IS NULL)),
  ADD CONSTRAINT comments_reply_kind CHECK (reply_kind IS NULL OR reply_kind IN ('reply','confirm','refute','supersede')),
  ADD CONSTRAINT comments_reply_target CHECK (reply_to_uid IS NULL OR (length(reply_to_uid) = 26 AND reply_to_uid <> uid));
CREATE INDEX idx_comments_reply_to ON comments(reply_to_uid) WHERE reply_to_uid IS NOT NULL;
