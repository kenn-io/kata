CREATE TABLE federation_retained_events (
  event_id BIGINT PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
  kept_local INTEGER NOT NULL DEFAULT 0
);
