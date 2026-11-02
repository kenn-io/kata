CREATE INDEX idx_comments_uid_handle ON comments((lower(substr(uid, 21))));
