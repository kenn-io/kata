CREATE INDEX idx_events_reply_to ON events((payload::jsonb->>'reply_to_uid')) WHERE type = 'issue.commented' AND (payload::jsonb->>'reply_to_uid') IS NOT NULL;
