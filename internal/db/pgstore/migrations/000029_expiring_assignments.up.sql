ALTER TABLE issues
  ADD COLUMN assignment_expires_on TEXT,
  ADD CONSTRAINT issues_assignment_expiry_requires_owner
    CHECK (assignment_expires_on IS NULL OR owner IS NOT NULL);

CREATE INDEX idx_issues_assignment_expires_on
  ON issues(assignment_expires_on, id)
  WHERE assignment_expires_on IS NOT NULL;
