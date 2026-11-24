ALTER TABLE federation_bindings ADD COLUMN retained_through_event_id BIGINT NOT NULL DEFAULT 0;

UPDATE federation_bindings b
SET retained_through_event_id = (
  SELECT COALESCE(MAX(e.id), 0) FROM events e
  WHERE e.project_id = b.project_id AND (e.origin_instance_uid = (SELECT value FROM meta WHERE key='instance_uid') OR EXISTS (SELECT 1 FROM meta WHERE key='former_instance_uid.' || e.origin_instance_uid))
)
WHERE b.role='spoke' AND b.relay_config IS NULL
AND EXISTS (
  SELECT 1 FROM federation_quarantine q JOIN events e ON e.project_id=q.project_id AND e.id BETWEEN q.first_event_id AND q.last_event_id
  WHERE q.project_id=b.project_id AND q.direction='push' AND q.skipped_at IS NULL
  AND e.actor<>b.bound_actor AND (e.origin_instance_uid = (SELECT value FROM meta WHERE key='instance_uid') OR EXISTS (SELECT 1 FROM meta WHERE key='former_instance_uid.' || e.origin_instance_uid))
  AND q.error LIKE '%event ' || e.uid || ' actor % does not match bound actor%'
);

UPDATE federation_quarantine q
SET skipped_at=now(), skipped_by='system', skip_reason='retry: retained history upgrade'
WHERE q.direction='push' AND q.skipped_at IS NULL AND EXISTS (
  SELECT 1 FROM federation_bindings b JOIN events e ON e.project_id=b.project_id AND e.id BETWEEN q.first_event_id AND q.last_event_id
  WHERE b.project_id=q.project_id AND b.role='spoke' AND b.relay_config IS NULL
  AND e.actor<>b.bound_actor AND (e.origin_instance_uid = (SELECT value FROM meta WHERE key='instance_uid') OR EXISTS (SELECT 1 FROM meta WHERE key='former_instance_uid.' || e.origin_instance_uid))
  AND q.error LIKE '%event ' || e.uid || ' actor % does not match bound actor%'
);
