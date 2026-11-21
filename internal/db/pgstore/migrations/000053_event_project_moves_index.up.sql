CREATE INDEX idx_events_project_moves ON events(project_id, id) WHERE type = 'issue.moved';
ANALYZE events;
