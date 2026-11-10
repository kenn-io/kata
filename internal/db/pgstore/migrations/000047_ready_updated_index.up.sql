CREATE INDEX idx_issues_ready_updated
  ON issues(updated_at DESC, id DESC)
  WHERE status = 'open' AND deleted_at IS NULL;
ANALYZE issues;
