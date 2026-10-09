
-- Teams are daemon-local actor groups; restricted empty policies stay restricted.
CREATE TABLE teams (
 uid TEXT PRIMARY KEY CHECK (length(uid)=26),
 name TEXT NOT NULL UNIQUE CHECK (length(trim(name))>0),
 revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0)
);
CREATE TABLE team_memberships (
 team_uid TEXT NOT NULL REFERENCES teams(uid) ON DELETE CASCADE,
 actor TEXT NOT NULL CHECK (length(trim(actor))>0),
 PRIMARY KEY(team_uid,actor)
);
CREATE INDEX idx_team_memberships_actor ON team_memberships(actor,team_uid);
CREATE TABLE project_access_policies (
 project_uid TEXT PRIMARY KEY REFERENCES projects(uid) ON DELETE CASCADE,
 visibility TEXT NOT NULL CHECK (visibility IN ('all','teams')),
 revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0)
);
CREATE TABLE project_access_teams (
 project_uid TEXT NOT NULL REFERENCES project_access_policies(project_uid) ON DELETE CASCADE,
 team_uid TEXT NOT NULL REFERENCES teams(uid) ON DELETE CASCADE,
 PRIMARY KEY(project_uid,team_uid)
);
CREATE INDEX idx_project_access_teams_team ON project_access_teams(team_uid,project_uid);
INSERT INTO meta(key,value) VALUES('project_access_revision','1');
