ALTER TABLE api_tokens
  ADD COLUMN scope_kind TEXT,
  ADD COLUMN scope_project_uid TEXT,
  ADD COLUMN scope_root_issue_uid TEXT,
  ADD COLUMN expires_at TEXT,
  ADD CONSTRAINT api_tokens_scope_shape CHECK (
    (scope_kind IS NULL AND scope_project_uid IS NULL AND scope_root_issue_uid IS NULL AND expires_at IS NULL)
    OR
    (scope_kind = 'issue_subtree' AND length(scope_project_uid) = 26
      AND length(scope_root_issue_uid) = 26 AND expires_at IS NOT NULL)
  );
