CREATE INDEX idx_events_resolved_issue_uid ON events (
    (CASE WHEN type LIKE 'issue.%' THEN COALESCE(NULLIF(issue_uid,''),payload::jsonb->>'issue_uid',payload::jsonb->>'uid') ELSE NULLIF(issue_uid,'') END),
    id
);
ANALYZE events;
