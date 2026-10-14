-- Project hub relay: team access policies, signed root attribution, project
-- relay transport state, and portable embedding artifacts.

-- Teams are daemon-local actor groups; restricted empty policies stay restricted.
CREATE TABLE teams (
 uid TEXT PRIMARY KEY,
 name TEXT NOT NULL UNIQUE,
 revision BIGINT NOT NULL DEFAULT 1
);
CREATE TABLE team_memberships (
 team_uid TEXT NOT NULL REFERENCES teams(uid) ON DELETE CASCADE,
 actor TEXT NOT NULL,
 PRIMARY KEY(team_uid,actor)
);
CREATE INDEX idx_team_memberships_actor ON team_memberships(actor,team_uid);
CREATE TABLE project_access_policies (
 project_uid TEXT PRIMARY KEY REFERENCES projects(uid) ON DELETE CASCADE,
 visibility TEXT NOT NULL,
 revision BIGINT NOT NULL DEFAULT 1
);
CREATE TABLE project_access_teams (
 project_uid TEXT NOT NULL REFERENCES project_access_policies(project_uid) ON DELETE CASCADE,
 team_uid TEXT NOT NULL REFERENCES teams(uid) ON DELETE CASCADE,
 PRIMARY KEY(project_uid,team_uid)
);
CREATE INDEX idx_project_access_teams_team ON project_access_teams(team_uid,project_uid);
INSERT INTO meta(key,value) VALUES('project_access_revision','1');

-- Root public-key pins and proofs are portable. Private signing material is not.
CREATE TABLE federation_root_keys (
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 authority_uid TEXT NOT NULL,
 key_id TEXT NOT NULL,
 public_key TEXT NOT NULL,
 active SMALLINT NOT NULL DEFAULT 1,
 PRIMARY KEY(project_uid,key_id)
);
CREATE UNIQUE INDEX uniq_federation_root_active ON federation_root_keys(project_uid) WHERE active=1;
CREATE TABLE federation_event_provenance (
 project_uid TEXT NOT NULL,
 event_uid TEXT NOT NULL,
 content_hash TEXT NOT NULL,
 reset_epoch BIGINT NOT NULL,
 sequence BIGINT NOT NULL,
 key_id TEXT NOT NULL,
 receipt TEXT NOT NULL,
 PRIMARY KEY(project_uid,event_uid),
 UNIQUE(project_uid,reset_epoch,sequence),
 FOREIGN KEY(project_uid,key_id) REFERENCES federation_root_keys(project_uid,key_id) ON DELETE CASCADE
);
CREATE TABLE federation_entity_provenance (
 project_uid TEXT NOT NULL,
 kind TEXT NOT NULL,
 entity_uid TEXT NOT NULL,
 event_uid TEXT NOT NULL,
 PRIMARY KEY(project_uid,kind,entity_uid),
 FOREIGN KEY(project_uid,event_uid) REFERENCES federation_event_provenance(project_uid,event_uid) ON DELETE CASCADE
);

-- Project-scoped transport grants remain linked to their human API credential.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_scope_shape;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_scope_shape CHECK (
 (scope_kind IS NULL AND scope_project_uid IS NULL AND scope_root_issue_uid IS NULL)
 OR (scope_kind='issue_subtree' AND length(scope_project_uid)=26 AND length(scope_root_issue_uid)=26 AND expires_at IS NOT NULL)
);
ALTER TABLE federation_bindings ADD COLUMN relay_config TEXT;
ALTER TABLE federation_enrollments ADD COLUMN relay_binding_uid TEXT UNIQUE;
ALTER TABLE federation_enrollments ADD COLUMN relay_protocol_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE federation_enrollments ADD COLUMN parent_token_id BIGINT REFERENCES api_tokens(id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE federation_enrollments ADD COLUMN relay_reset_epoch BIGINT NOT NULL DEFAULT 1;
ALTER TABLE federation_enrollments ADD COLUMN relay_serve_downstream INTEGER NOT NULL DEFAULT 0;

-- Per-hop source mappings retain retry identities independently of source history.
CREATE TABLE federation_relay_outbox (
 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 binding_uid TEXT NOT NULL,
 stream TEXT NOT NULL,
 reset_epoch BIGINT NOT NULL,
 source_uid TEXT NOT NULL,
 source_hash TEXT NOT NULL,
 envelope TEXT NOT NULL,
 emitted INTEGER NOT NULL DEFAULT 0,
 acknowledged INTEGER NOT NULL DEFAULT 0,
 UNIQUE(binding_uid,stream,reset_epoch,source_uid)
);
CREATE INDEX idx_relay_outbox_pending ON federation_relay_outbox(binding_uid,stream,reset_epoch,id) WHERE acknowledged=0;

-- Offers remain unresolved until complete data is durably committed. Numeric
-- sequence gaps are valid; the recorded offer order defines the accepted prefix.
CREATE TABLE federation_relay_inbox (
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 binding_uid TEXT NOT NULL,
 stream TEXT NOT NULL,
 reset_epoch BIGINT NOT NULL,
 sequence BIGINT NOT NULL,
 source_uid TEXT NOT NULL,
 source_hash TEXT NOT NULL,
 envelope_digest TEXT NOT NULL,
 envelope TEXT NOT NULL,
 accepted INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(binding_uid,stream,reset_epoch,sequence),
 UNIQUE(binding_uid,stream,reset_epoch,source_uid)
);
CREATE INDEX idx_relay_inbox_prefix ON federation_relay_inbox(binding_uid,stream,reset_epoch,sequence) WHERE accepted=0;
CREATE TABLE federation_relay_cursors (
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 binding_uid TEXT NOT NULL,
 stream TEXT NOT NULL,
 reset_epoch BIGINT NOT NULL,
 offered_through BIGINT NOT NULL DEFAULT 0,
 accepted_through BIGINT NOT NULL DEFAULT 0,
 emitted_through BIGINT NOT NULL DEFAULT 0,
 acknowledged_through BIGINT NOT NULL DEFAULT 0,
 PRIMARY KEY(binding_uid,stream,reset_epoch)
);

-- Portable original float32 artifacts are separate from the local vector index.
-- Non-null staging_expires_at never establishes durable relay acceptance.
CREATE TABLE federation_embedding_artifacts (
  project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
  digest TEXT NOT NULL,
  issue_uid TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  recipe_fingerprint TEXT NOT NULL,
  vector_byte_size BIGINT NOT NULL,
  manifest TEXT NOT NULL,
  artifact TEXT NOT NULL,
  staging_expires_at TEXT,
  PRIMARY KEY(project_uid,digest)
);
CREATE INDEX idx_embedding_artifacts_input ON federation_embedding_artifacts(project_uid,issue_uid,input_hash,recipe_fingerprint);
CREATE INDEX idx_embedding_artifacts_staging ON federation_embedding_artifacts(project_uid,staging_expires_at) WHERE staging_expires_at IS NOT NULL;

