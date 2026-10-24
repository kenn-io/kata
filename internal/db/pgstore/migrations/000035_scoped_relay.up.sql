-- Scoped v2 projection and custody state. Go validates shapes; obligations never cascade with grants/projects.
CREATE TABLE federation_scoped_views (
 view_uid TEXT NOT NULL,
 catalog_entry_uid TEXT NOT NULL,
 authority_uid TEXT NOT NULL,
 binding_uid TEXT NOT NULL,
 epoch BIGINT NOT NULL,
 accountable_actor TEXT NOT NULL,
 route_digest TEXT NOT NULL,
 destination_uid TEXT NOT NULL,
 active_generation BIGINT,
 prepared_generation BIGINT,
 state TEXT NOT NULL,
 reason_code TEXT NOT NULL,
 request_digest TEXT NOT NULL,
 PRIMARY KEY(view_uid)
);
CREATE TABLE federation_scoped_generations (
 view_uid TEXT NOT NULL,
 generation BIGINT NOT NULL,
 descriptor_bytes BYTEA NOT NULL,
 descriptor_digest TEXT NOT NULL,
 checkpoint_bytes BYTEA,
 checkpoint_digest TEXT,
 cut_sequence BIGINT NOT NULL,
 cut_digest TEXT NOT NULL,
 state TEXT NOT NULL,
 assembled_bytes BIGINT NOT NULL,
 root_cutover_uid TEXT NOT NULL,
 PRIMARY KEY(view_uid,generation),
 FOREIGN KEY(view_uid) REFERENCES federation_scoped_views(view_uid)
);
CREATE TABLE federation_scoped_projects (
 project_uid TEXT NOT NULL,
 owner_view_uid TEXT,
 owner_generation BIGINT,
 protocol_floor BIGINT NOT NULL,
 root_cutover_uid TEXT,
 conversion_uid TEXT,
 serving_fenced INTEGER NOT NULL,
 PRIMARY KEY(project_uid)
);
CREATE TABLE federation_scoped_records (
 view_uid TEXT NOT NULL,
 generation BIGINT NOT NULL,
 collection TEXT NOT NULL,
 record_key TEXT NOT NULL,
 project_uid TEXT NOT NULL,
 subject_uid TEXT,
 peer_project_uid TEXT,
 peer_uid TEXT,
 visible_revision BIGINT NOT NULL,
 present INTEGER NOT NULL,
 record_bytes BYTEA NOT NULL,
 digest TEXT NOT NULL,
 PRIMARY KEY(view_uid,generation,collection,record_key),
 FOREIGN KEY(view_uid,generation) REFERENCES federation_scoped_generations(view_uid,generation)
);
CREATE TABLE federation_scoped_batches (
 view_uid TEXT NOT NULL,
 generation BIGINT NOT NULL,
 sequence BIGINT NOT NULL,
 digest TEXT NOT NULL,
 previous_digest TEXT NOT NULL,
 capture_uid TEXT NOT NULL,
 batch_bytes BYTEA NOT NULL,
 PRIMARY KEY(view_uid,generation,sequence),
 FOREIGN KEY(view_uid,generation) REFERENCES federation_scoped_generations(view_uid,generation)
);
CREATE TABLE federation_scoped_work (
 view_uid TEXT NOT NULL,
 dirty_revision BIGINT NOT NULL,
 captured_revision BIGINT,
 capture_uid TEXT,
 capture_bytes BYTEA,
 capture_digest TEXT,
 state TEXT NOT NULL,
 PRIMARY KEY(view_uid),
 FOREIGN KEY(view_uid) REFERENCES federation_scoped_views(view_uid)
);
CREATE TABLE federation_scoped_delivery (
 binding_uid TEXT NOT NULL,
 epoch BIGINT NOT NULL,
 direction TEXT NOT NULL,
 channel TEXT NOT NULL,
 sequence BIGINT NOT NULL,
 object_uid TEXT NOT NULL,
 object_digest TEXT NOT NULL,
 offered_digest TEXT NOT NULL,
 header_bytes BYTEA NOT NULL,
 envelope_bytes BYTEA,
 accepted INTEGER NOT NULL,
 emitted INTEGER NOT NULL,
 acknowledged INTEGER NOT NULL,
 upstream_binding_uid TEXT,
 upstream_epoch BIGINT,
 upstream_direction TEXT,
 upstream_channel TEXT,
 upstream_sequence BIGINT,
 PRIMARY KEY(binding_uid,epoch,direction,channel,sequence)
);
CREATE TABLE federation_scoped_cursors (
 binding_uid TEXT NOT NULL,
 epoch BIGINT NOT NULL,
 direction TEXT NOT NULL,
 channel TEXT NOT NULL,
 received_sequence BIGINT NOT NULL,
 received_digest TEXT NOT NULL,
 accepted_sequence BIGINT NOT NULL,
 accepted_digest TEXT NOT NULL,
 emitted_sequence BIGINT NOT NULL,
 emitted_digest TEXT NOT NULL,
 acknowledged_sequence BIGINT NOT NULL,
 acknowledged_digest TEXT NOT NULL,
 PRIMARY KEY(binding_uid,epoch,direction,channel)
);
CREATE TABLE federation_source_operations (
 operation_uid TEXT NOT NULL,
 digest TEXT NOT NULL,
 source_instance_uid TEXT NOT NULL,
 command_kind TEXT NOT NULL,
 manifest_bytes BYTEA NOT NULL,
 principal_bytes BYTEA NOT NULL,
 principal_key TEXT NOT NULL,
 state TEXT NOT NULL,
 overlay_state TEXT NOT NULL,
 admission_bytes BYTEA,
 terminal_bytes BYTEA,
 cancel_requested INTEGER NOT NULL,
 idempotency_key TEXT,
 request_fingerprint TEXT NOT NULL,
 PRIMARY KEY(operation_uid)
);
CREATE TABLE federation_private_sources (
 event_uid TEXT NOT NULL,
 content_hash TEXT NOT NULL,
 project_uid TEXT NOT NULL,
 operation_uid TEXT,
 ordinal BIGINT,
 conversion_uid TEXT,
 event_bytes BYTEA NOT NULL,
 provenance_bytes BYTEA,
 PRIMARY KEY(event_uid),
 FOREIGN KEY(operation_uid) REFERENCES federation_source_operations(operation_uid)
);
CREATE TABLE federation_source_custody (
 operation_uid TEXT NOT NULL,
 binding_uid TEXT NOT NULL,
 epoch BIGINT NOT NULL,
 direction TEXT NOT NULL,
 principal_bytes BYTEA NOT NULL,
 manifest_digest TEXT NOT NULL,
 owns_complete INTEGER NOT NULL,
 result_acknowledged INTEGER NOT NULL,
 invalidation_state TEXT NOT NULL,
 PRIMARY KEY(operation_uid,binding_uid,epoch,direction),
 FOREIGN KEY(operation_uid) REFERENCES federation_source_operations(operation_uid)
);
CREATE TABLE federation_source_dependencies (
 operation_uid TEXT NOT NULL,
 project_uid TEXT NOT NULL,
 issue_uid TEXT NOT NULL,
 establishing_operation_uid TEXT,
 proof_bytes BYTEA NOT NULL,
 state TEXT NOT NULL,
 terminal_reason TEXT,
 PRIMARY KEY(operation_uid,project_uid,issue_uid),
 FOREIGN KEY(operation_uid) REFERENCES federation_source_operations(operation_uid)
);
CREATE TABLE federation_source_coverage (
 operation_uid TEXT NOT NULL,
 view_uid TEXT NOT NULL,
 generation BIGINT NOT NULL,
 cut_digest TEXT NOT NULL,
 outcome TEXT NOT NULL,
 certificate_bytes BYTEA NOT NULL,
 PRIMARY KEY(operation_uid,view_uid,generation,cut_digest),
 FOREIGN KEY(operation_uid) REFERENCES federation_source_operations(operation_uid),
 FOREIGN KEY(view_uid,generation) REFERENCES federation_scoped_generations(view_uid,generation)
);
CREATE TABLE federation_control_custody (
 request_uid TEXT NOT NULL,
 binding_uid TEXT NOT NULL,
 request_digest TEXT NOT NULL,
 principal_bytes BYTEA NOT NULL,
 request_bytes BYTEA NOT NULL,
 result_bytes BYTEA,
 state TEXT NOT NULL,
 result_acknowledged INTEGER NOT NULL,
 conversion_uid TEXT,
 PRIMARY KEY(request_uid,binding_uid)
);
CREATE TABLE federation_mutation_expectations (
 token_digest TEXT NOT NULL,
 project_uid TEXT NOT NULL,
 subject_uid TEXT NOT NULL,
 predicate_kind TEXT NOT NULL,
 predicate_bytes BYTEA NOT NULL,
 principal_bytes BYTEA NOT NULL,
 route_digest TEXT NOT NULL,
 nonce TEXT NOT NULL,
 expires_at TEXT NOT NULL,
 consumed_operation_uid TEXT,
 PRIMARY KEY(token_digest)
);
CREATE TABLE federation_predicate_incarnations (
 project_uid TEXT NOT NULL,
 subject_uid TEXT NOT NULL,
 predicate_kind TEXT NOT NULL,
 incarnation BIGINT NOT NULL,
 canonical_digest TEXT NOT NULL,
 PRIMARY KEY(project_uid,subject_uid,predicate_kind)
);
CREATE TABLE federation_canonical_close_state (
 issue_uid TEXT NOT NULL,
 project_uid TEXT NOT NULL,
 status_intent_uid TEXT,
 status_clock_bytes BYTEA NOT NULL,
 value_bytes BYTEA,
 PRIMARY KEY(issue_uid)
);
CREATE TABLE federation_protocol_cutovers (
 cutover_uid TEXT NOT NULL,
 request_digest TEXT NOT NULL,
 project_uids_bytes BYTEA NOT NULL,
 floor BIGINT NOT NULL,
 readiness_bytes BYTEA NOT NULL,
 suspended_bindings_bytes BYTEA NOT NULL,
 certificate_bytes BYTEA NOT NULL,
 state TEXT NOT NULL,
 PRIMARY KEY(cutover_uid)
);
CREATE TABLE federation_scoped_conversions (
 conversion_uid TEXT NOT NULL,
 request_digest TEXT NOT NULL,
 request_bytes BYTEA NOT NULL,
 old_bindings_bytes BYTEA NOT NULL,
 root_cutover_uid TEXT,
 inventory_digest TEXT,
 baseline_digest TEXT,
 phase TEXT NOT NULL,
 result_bytes BYTEA,
 PRIMARY KEY(conversion_uid)
);
CREATE TABLE federation_conversion_obligations (
 conversion_uid TEXT NOT NULL,
 obligation_uid TEXT NOT NULL,
 kind TEXT NOT NULL,
 binding_uid TEXT NOT NULL,
 object_uid TEXT NOT NULL,
 digest TEXT NOT NULL,
 epoch BIGINT NOT NULL,
 sequence BIGINT NOT NULL,
 principal_key TEXT NOT NULL,
 state TEXT NOT NULL,
 PRIMARY KEY(conversion_uid,obligation_uid),
 FOREIGN KEY(conversion_uid) REFERENCES federation_scoped_conversions(conversion_uid)
);
CREATE UNIQUE INDEX uniq_scoped_view_selection ON federation_scoped_views(catalog_entry_uid,authority_uid,binding_uid,epoch,accountable_actor,route_digest,destination_uid);
CREATE INDEX idx_scoped_generation_state ON federation_scoped_generations(view_uid,state);
CREATE INDEX idx_scoped_project_owner ON federation_scoped_projects(owner_view_uid,owner_generation);
CREATE INDEX idx_scoped_record_project ON federation_scoped_records(view_uid,generation,project_uid,collection);
CREATE INDEX idx_scoped_record_peer ON federation_scoped_records(view_uid,generation,peer_uid);
CREATE UNIQUE INDEX uniq_scoped_batch_digest ON federation_scoped_batches(view_uid,generation,digest);
CREATE INDEX idx_scoped_work_pending ON federation_scoped_work(state,view_uid);
CREATE INDEX idx_scoped_delivery_outbound ON federation_scoped_delivery(binding_uid,epoch,direction,channel,sequence) WHERE direction='outbound' AND acknowledged=0;
CREATE INDEX idx_scoped_delivery_inbound ON federation_scoped_delivery(binding_uid,epoch,direction,channel,sequence) WHERE direction='inbound' AND accepted=0;
CREATE INDEX idx_source_operation_state ON federation_source_operations(state,operation_uid);
CREATE UNIQUE INDEX uniq_source_operation_retry ON federation_source_operations(source_instance_uid,principal_key,idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key<>'';
CREATE UNIQUE INDEX uniq_private_source_ordinal ON federation_private_sources(operation_uid,ordinal) WHERE operation_uid IS NOT NULL;
CREATE INDEX idx_private_source_conversion ON federation_private_sources(conversion_uid,project_uid);
CREATE INDEX idx_source_custody_result ON federation_source_custody(binding_uid,result_acknowledged);
CREATE INDEX idx_source_dependency_establishment ON federation_source_dependencies(establishing_operation_uid,state);
CREATE INDEX idx_source_coverage_view ON federation_source_coverage(view_uid,generation);
CREATE INDEX idx_control_custody_state ON federation_control_custody(state,binding_uid);
CREATE INDEX idx_mutation_expectation_subject ON federation_mutation_expectations(subject_uid,predicate_kind);
CREATE INDEX idx_canonical_close_project ON federation_canonical_close_state(project_uid,issue_uid);
CREATE INDEX idx_protocol_cutover_state ON federation_protocol_cutovers(state,cutover_uid);
CREATE INDEX idx_scoped_conversion_phase ON federation_scoped_conversions(phase,conversion_uid);
CREATE INDEX idx_conversion_obligation_state ON federation_conversion_obligations(conversion_uid,state);
