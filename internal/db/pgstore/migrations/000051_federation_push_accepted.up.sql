CREATE TABLE federation_push_accepted (
  project_id BIGINT NOT NULL REFERENCES federation_bindings(project_id) ON DELETE CASCADE,
  event_id BIGINT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  PRIMARY KEY (project_id, event_id)
);

CREATE TABLE federation_local_replays (
  event_uid    TEXT PRIMARY KEY REFERENCES events(uid) ON DELETE CASCADE,
  delivery_due INTEGER NOT NULL DEFAULT 0
);

-- An interrupted reset leaves local-origin events restored after the reset
-- with no pull completed since. Record them so the next pull still delivers them,
-- except acknowledged events whose issue is projected, which already notified.
INSERT INTO federation_local_replays(event_uid, delivery_due)
SELECT e.uid, CASE WHEN e.id > b.push_cursor_event_id OR (e.issue_id IS NULL AND e.issue_uid IS NOT NULL) THEN 1 ELSE 0 END
  FROM events e
  JOIN federation_bindings b ON b.project_id = e.project_id
  JOIN federation_sync_status s ON s.project_id = e.project_id
 WHERE e.origin_instance_uid = (SELECT value FROM meta WHERE key='instance_uid')
   AND b.pull_cursor_event_id = b.replay_horizon_event_id - 1
   AND s.last_reset_at IS NOT NULL
   AND e.created_at < s.last_reset_at
   AND (s.last_pull_success_at IS NULL OR s.last_pull_success_at < s.last_reset_at);
