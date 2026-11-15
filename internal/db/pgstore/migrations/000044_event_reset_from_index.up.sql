CREATE INDEX idx_events_reset_from ON events((payload::jsonb->>'from_project_uid'), (payload::jsonb->>'from_short_id')) WHERE type = 'sync.reset_required';
