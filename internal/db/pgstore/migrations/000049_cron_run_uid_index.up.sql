CREATE INDEX idx_events_cron_run_uid ON events(project_id, type, (payload::jsonb->>'uid')) WHERE type LIKE 'cron.%';
