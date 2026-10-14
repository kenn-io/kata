CREATE INDEX idx_comments_reply_to ON comments(reply_to_uid) WHERE reply_to_uid IS NOT NULL;
