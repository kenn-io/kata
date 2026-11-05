CREATE INDEX idx_issues_uid_prefix ON issues(uid text_pattern_ops);
CREATE INDEX idx_issues_project_uid_prefix ON issues(project_id, uid text_pattern_ops);
ANALYZE issues;
