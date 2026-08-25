SELECT events.id, events.project_id, projects.name, events.issue_id, events.issue_number,
	                 CASE WHEN (peer.id IS NULL AND events.related_issue_id IS NOT NULL) THEN NULL ELSE events.related_issue_id END, events.type, events.actor, events.payload, CAST(events.created_at AS TEXT)
	          FROM events JOIN projects ON projects.id = events.project_id
	          LEFT JOIN issues subject_issue ON subject_issue.id = events.issue_id
	          LEFT JOIN issues peer ON peer.id = events.related_issue_id WHERE (events.issue_id IS NULL OR subject_issue.id IS NOT NULL) AND events.project_id = ? ORDER BY events.id ASC