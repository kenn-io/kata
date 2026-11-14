DROP INDEX idx_events_resolved_issue_uid;
CREATE INDEX idx_events_issue_uid_id ON events(issue_uid, id) WHERE issue_uid IS NOT NULL;
ANALYZE events;
