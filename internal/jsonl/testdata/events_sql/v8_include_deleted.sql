SELECT events.id, events.uid, events.origin_instance_uid, events.project_id, events.project_name, events.issue_id, events.issue_uid,
	                 CASE WHEN (peer.id IS NULL AND events.related_issue_id IS NOT NULL) THEN NULL ELSE events.related_issue_id END, CASE WHEN (peer.id IS NULL AND events.related_issue_id IS NOT NULL) THEN NULL ELSE events.related_issue_uid END,
	                 events.type, events.actor, events.payload, CAST(events.created_at AS TEXT)
	          FROM events
	          LEFT JOIN issues subject_issue ON subject_issue.id = events.issue_id
	          LEFT JOIN issues peer ON peer.id = events.related_issue_id WHERE (events.issue_id IS NULL OR subject_issue.id IS NOT NULL) AND events.project_id = ? ORDER BY events.id ASC