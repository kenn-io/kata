CREATE INDEX idx_issues_project_created
  ON issues(project_id, created_at, id) WHERE deleted_at IS NULL;
CREATE INDEX idx_issues_created
  ON issues(created_at, id) WHERE deleted_at IS NULL;
ANALYZE issues;
