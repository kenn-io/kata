CREATE TABLE federation_local_replays (
  event_uid TEXT PRIMARY KEY REFERENCES events(uid) ON DELETE CASCADE
);

-- An interrupted reset leaves local-origin events restored after the reset
-- with no pull completed since. Hub acknowledgment does not establish delivery.
INSERT INTO federation_local_replays(event_uid)
SELECT e.uid
  FROM events e
  JOIN federation_bindings b ON b.project_id = e.project_id
  JOIN federation_sync_status s ON s.project_id = e.project_id
 WHERE e.origin_instance_uid = (SELECT value FROM meta WHERE key='instance_uid')
   AND b.pull_cursor_event_id = b.replay_horizon_event_id - 1
   AND s.last_reset_at IS NOT NULL
   AND e.created_at <= s.last_reset_at
   AND (s.last_pull_success_at IS NULL OR s.last_pull_success_at < s.last_reset_at);

CREATE TABLE federation_push_accepted (
  project_id BIGINT NOT NULL REFERENCES federation_bindings(project_id) ON DELETE CASCADE,
  event_id BIGINT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  PRIMARY KEY (project_id, event_id)
);
