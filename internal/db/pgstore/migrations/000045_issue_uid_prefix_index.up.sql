CREATE INDEX idx_issues_uid_prefix ON issues(uid text_pattern_ops);
ANALYZE issues;
