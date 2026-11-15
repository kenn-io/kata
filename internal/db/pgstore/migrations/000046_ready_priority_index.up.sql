CREATE INDEX idx_issues_project_ready_priority
  ON issues(project_id, COALESCE(priority, 5), updated_at DESC, id DESC)
  WHERE status = 'open' AND deleted_at IS NULL;
CREATE INDEX idx_issues_ready_priority
  ON issues(COALESCE(priority, 5), updated_at DESC, id DESC)
  WHERE status = 'open' AND deleted_at IS NULL;
ANALYZE issues;
