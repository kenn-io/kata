CREATE INDEX idx_events_moved_from ON events((payload::jsonb->>'from_project_uid'), (payload::jsonb->>'from_short_id')) WHERE type = 'issue.moved';
