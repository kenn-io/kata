CREATE INDEX idx_events_moved_from_id ON events((payload::jsonb->>'from_project_uid'), id) WHERE type = 'issue.moved';
