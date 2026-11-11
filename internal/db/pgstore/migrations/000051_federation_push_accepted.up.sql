CREATE TABLE federation_push_accepted (
  project_id BIGINT NOT NULL REFERENCES federation_bindings(project_id) ON DELETE CASCADE,
  event_id BIGINT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  PRIMARY KEY (project_id, event_id)
);
