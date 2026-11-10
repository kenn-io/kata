CREATE TABLE federation_local_replays (
  event_uid TEXT PRIMARY KEY REFERENCES events(uid) ON DELETE CASCADE
);
