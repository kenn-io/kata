SELECT events.id, events.uid, events.origin_instance_uid, events.project_id, export_project.uid, events.project_name, CASE WHEN subject_issue.id IS NOT NULL AND subject_issue.project_id <> ? THEN NULL ELSE events.issue_id END, events.issue_uid,
	                 CASE WHEN (peer.id IS NULL AND (events.related_issue_id IS NOT NULL OR events.related_issue_uid IS NOT NULL)) OR (peer.id IS NOT NULL AND peer.project_id <> ?) THEN NULL ELSE events.related_issue_id END,
	                 CASE WHEN (peer.id IS NULL AND (events.related_issue_id IS NOT NULL OR events.related_issue_uid IS NOT NULL)) OR (peer.id IS NOT NULL AND peer.project_id <> ?) THEN NULL ELSE events.related_issue_uid END,
	                 events.type, events.actor, events.payload, events.hlc_physical_ms, events.hlc_counter, events.content_hash,
	                 CAST(events.created_at AS TEXT)
	          FROM events
	          JOIN projects export_project ON export_project.id = events.project_id
	          LEFT JOIN issues subject_issue ON subject_issue.id = events.issue_id
	               OR (events.issue_id IS NULL AND events.issue_uid IS NOT NULL AND subject_issue.uid = events.issue_uid)
	          LEFT JOIN issues peer ON peer.id = events.related_issue_id
	               OR (events.related_issue_id IS NULL AND events.related_issue_uid IS NOT NULL AND peer.uid = events.related_issue_uid) WHERE (events.issue_id IS NULL OR subject_issue.id IS NOT NULL) AND events.project_id = ? ORDER BY events.id ASC