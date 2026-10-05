-- Project-scoped transport grants remain linked to their human API credential.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_scope_shape;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_scope_shape CHECK (
 (scope_kind IS NULL AND scope_project_uid IS NULL AND scope_root_issue_uid IS NULL)
 OR (scope_kind='issue_subtree' AND length(scope_project_uid)=26 AND length(scope_root_issue_uid)=26 AND expires_at IS NOT NULL)
);
ALTER TABLE federation_bindings ADD COLUMN relay_config TEXT CHECK(relay_config IS NULL OR jsonb_typeof(relay_config::jsonb)='object');
ALTER TABLE federation_enrollments ADD COLUMN relay_binding_uid TEXT UNIQUE;
ALTER TABLE federation_enrollments ADD COLUMN relay_protocol_version INTEGER NOT NULL DEFAULT 0 CHECK(relay_protocol_version IN (0,1));
ALTER TABLE federation_enrollments ADD COLUMN parent_token_id BIGINT REFERENCES api_tokens(id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE federation_enrollments ADD COLUMN relay_reset_epoch BIGINT NOT NULL DEFAULT 1 CHECK(relay_reset_epoch>0);
ALTER TABLE federation_enrollments ADD COLUMN relay_serve_downstream INTEGER NOT NULL DEFAULT 0 CHECK(relay_serve_downstream IN (0,1));
ALTER TABLE federation_enrollments ADD CONSTRAINT federation_enrollments_relay_shape CHECK (
 (relay_protocol_version=0 AND relay_binding_uid IS NULL AND parent_token_id IS NULL AND relay_serve_downstream=0)
 OR (relay_protocol_version=1 AND relay_binding_uid IS NOT NULL AND length(relay_binding_uid)=26 AND parent_token_id IS NOT NULL AND project_id IS NOT NULL)
);

-- Per-hop source mappings retain retry identities independently of source history.
CREATE TABLE federation_relay_outbox (
 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 binding_uid TEXT NOT NULL CHECK(length(binding_uid)=26),
 stream TEXT NOT NULL CHECK(stream IN ('events','receipts','artifacts')),
 reset_epoch BIGINT NOT NULL CHECK(reset_epoch>0),
 source_uid TEXT NOT NULL,
 source_hash TEXT NOT NULL CHECK(length(source_hash)=64),
 envelope TEXT NOT NULL,
 emitted INTEGER NOT NULL DEFAULT 0 CHECK(emitted IN (0,1)),
 acknowledged INTEGER NOT NULL DEFAULT 0 CHECK(acknowledged IN (0,1)),
 UNIQUE(binding_uid,stream,reset_epoch,source_uid),
 CHECK(acknowledged=0 OR emitted=1)
);
CREATE INDEX idx_relay_outbox_pending ON federation_relay_outbox(binding_uid,stream,reset_epoch,id) WHERE acknowledged=0;

-- Offers remain unresolved until complete data is durably committed. Numeric
-- sequence gaps are valid; the recorded offer order defines the accepted prefix.
CREATE TABLE federation_relay_inbox (
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 binding_uid TEXT NOT NULL CHECK(length(binding_uid)=26),
 stream TEXT NOT NULL CHECK(stream IN ('events','receipts','artifacts')),
 reset_epoch BIGINT NOT NULL CHECK(reset_epoch>0),
 sequence BIGINT NOT NULL CHECK(sequence>0),
 source_uid TEXT NOT NULL,
 source_hash TEXT NOT NULL CHECK(length(source_hash)=64),
 envelope_digest TEXT NOT NULL CHECK(length(envelope_digest)=64),
 envelope TEXT NOT NULL,
 accepted INTEGER NOT NULL DEFAULT 0 CHECK(accepted IN (0,1)),
 PRIMARY KEY(binding_uid,stream,reset_epoch,sequence),
 UNIQUE(binding_uid,stream,reset_epoch,source_uid)
);
CREATE INDEX idx_relay_inbox_prefix ON federation_relay_inbox(binding_uid,stream,reset_epoch,sequence) WHERE accepted=0;
CREATE TABLE federation_relay_cursors (
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 binding_uid TEXT NOT NULL CHECK(length(binding_uid)=26),
 stream TEXT NOT NULL CHECK(stream IN ('events','receipts','artifacts')),
 reset_epoch BIGINT NOT NULL CHECK(reset_epoch>0),
 offered_through BIGINT NOT NULL DEFAULT 0 CHECK(offered_through>=0),
 accepted_through BIGINT NOT NULL DEFAULT 0 CHECK(accepted_through>=0 AND accepted_through<=offered_through),
 emitted_through BIGINT NOT NULL DEFAULT 0 CHECK(emitted_through>=0),
 acknowledged_through BIGINT NOT NULL DEFAULT 0 CHECK(acknowledged_through>=0 AND acknowledged_through<=emitted_through),
 PRIMARY KEY(binding_uid,stream,reset_epoch)
);
