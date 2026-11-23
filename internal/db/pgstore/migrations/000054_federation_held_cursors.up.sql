CREATE TABLE federation_held_cursors (
  project_uid TEXT PRIMARY KEY,
  push_cursor_event_id BIGINT NOT NULL
);
