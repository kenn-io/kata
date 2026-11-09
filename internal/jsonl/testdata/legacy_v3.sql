CREATE TABLE projects (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE,
  identity TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  next_issue_number INTEGER NOT NULL DEFAULT 1,
  CHECK (length(uid) = 26)
);
-- v3 projects: no deleted_at column and no idx_projects_active index
-- (added by the v3->v4 cutover).
CREATE TABLE project_aliases (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  alias_identity TEXT UNIQUE NOT NULL,
  alias_kind TEXT NOT NULL,
  root_path TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  last_seen_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE issues (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  number INTEGER NOT NULL,
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'open',
  closed_reason TEXT,
  owner TEXT,
  author TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  closed_at DATETIME,
  deleted_at DATETIME,
  UNIQUE(project_id, number),
  CHECK (length(uid) = 26)
);
CREATE TABLE comments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_id INTEGER NOT NULL REFERENCES issues(id),
  author TEXT NOT NULL,
  body TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE links (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  from_issue_id INTEGER NOT NULL REFERENCES issues(id),
  to_issue_id INTEGER NOT NULL REFERENCES issues(id),
  from_issue_uid TEXT NOT NULL,
  to_issue_uid TEXT NOT NULL,
  type TEXT NOT NULL,
  author TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE issue_labels (
  issue_id INTEGER NOT NULL REFERENCES issues(id),
  label TEXT NOT NULL,
  author TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY(issue_id, label)
);
-- v3 events: gain uid + origin_instance_uid (the v2->v3 cutover backfilled them).
CREATE TABLE events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE,
  origin_instance_uid TEXT NOT NULL,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  project_identity TEXT NOT NULL,
  issue_id INTEGER REFERENCES issues(id),
  issue_uid TEXT,
  issue_number INTEGER,
  related_issue_id INTEGER REFERENCES issues(id),
  related_issue_uid TEXT,
  type TEXT NOT NULL,
  actor TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '{}',
  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
-- v3 purge_log: gain uid + origin_instance_uid.
CREATE TABLE purge_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE,
  origin_instance_uid TEXT NOT NULL,
  project_id INTEGER NOT NULL,
  purged_issue_id INTEGER NOT NULL,
  issue_uid TEXT,
  project_uid TEXT,
  project_identity TEXT NOT NULL,
  issue_number INTEGER NOT NULL,
  issue_title TEXT NOT NULL,
  issue_author TEXT NOT NULL,
  comment_count INTEGER NOT NULL,
  link_count INTEGER NOT NULL,
  label_count INTEGER NOT NULL,
  event_count INTEGER NOT NULL,
  events_deleted_min_id INTEGER,
  events_deleted_max_id INTEGER,
  purge_reset_after_event_id INTEGER,
  actor TEXT NOT NULL,
  reason TEXT,
  purged_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO meta(key, value) VALUES('schema_version', '3');
INSERT INTO meta(key, value) VALUES('created_by_version', '0.1.0');
INSERT INTO meta(key, value) VALUES('instance_uid', '01HZZZZZZZZZZZZZZZZZZZZZ01');

INSERT INTO projects(id, uid, identity, name, created_at, next_issue_number)
VALUES(1, '01HZZZZZZZZZZZZZZZZZZZZ001', 'proj-a', 'Proj A', '2026-05-03T00:00:00.000Z', 1);
INSERT INTO sqlite_sequence(name, seq) VALUES('projects', 1);
