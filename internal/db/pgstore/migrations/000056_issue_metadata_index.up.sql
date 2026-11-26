CREATE INDEX idx_issues_metadata ON issues USING GIN ((metadata::jsonb));
ANALYZE issues;
