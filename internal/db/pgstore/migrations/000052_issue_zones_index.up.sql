CREATE INDEX idx_issues_zone_candidates ON issues(project_id) WHERE ((json(metadata) ->> 'scheduled_on') LIKE '%]' OR (json(metadata) ->> 'deadline_on') LIKE '%]');
ANALYZE issues;
