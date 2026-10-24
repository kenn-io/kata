package jsonl

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"go.kenn.io/kata/internal/db"
	"slices"
)

func decodeScopedImportRecord(env Envelope) (db.ImportRecord, bool, error) {
	var record db.ImportRecord
	var required, nullable []string
	switch env.Kind {
	case Kind("federation_scoped_view"):
		record = &db.ScopedViewExport{}
		nullable = []string{"active_generation", "prepared_generation"}
		required = []string{"view_uid", "catalog_entry_uid", "authority_uid", "binding_uid", "epoch", "accountable_actor", "route_digest", "destination_uid", "active_generation", "prepared_generation", "state", "reason_code", "request_digest"}
	case Kind("federation_scoped_generation"):
		record = &db.ScopedGenerationExport{}
		nullable = []string{"checkpoint_bytes", "checkpoint_digest"}
		required = []string{"view_uid", "generation", "descriptor_bytes", "descriptor_digest", "checkpoint_bytes", "checkpoint_digest", "cut_sequence", "cut_digest", "state", "assembled_bytes", "root_cutover_uid"}
	case Kind("federation_scoped_project"):
		record = &db.ScopedProjectExport{}
		nullable = []string{"owner_view_uid", "owner_generation", "root_cutover_uid", "conversion_uid"}
		required = []string{"project_uid", "owner_view_uid", "owner_generation", "protocol_floor", "root_cutover_uid", "conversion_uid", "serving_fenced"}
	case Kind("federation_scoped_record"):
		record = &db.ScopedRecordExport{}
		nullable = []string{"subject_uid", "peer_project_uid", "peer_uid"}
		required = []string{"view_uid", "generation", "collection", "record_key", "project_uid", "subject_uid", "peer_project_uid", "peer_uid", "visible_revision", "present", "record_bytes", "digest"}
	case Kind("federation_scoped_batch"):
		record = &db.ScopedBatchExport{}
		nullable = []string{}
		required = []string{"view_uid", "generation", "sequence", "digest", "previous_digest", "capture_uid", "batch_bytes"}
	case Kind("federation_scoped_work"):
		record = &db.ScopedWorkExport{}
		nullable = []string{"captured_revision", "capture_uid", "capture_bytes", "capture_digest"}
		required = []string{"view_uid", "dirty_revision", "captured_revision", "capture_uid", "capture_bytes", "capture_digest", "state"}
	case Kind("federation_scoped_delivery"):
		record = &db.ScopedDeliveryRecord{}
		nullable = []string{"envelope_bytes", "upstream"}
		required = []string{"binding_uid", "epoch", "direction", "channel", "sequence", "object_uid", "object_digest", "offered_digest", "header_bytes", "envelope_bytes", "accepted", "emitted", "acknowledged", "upstream"}
	case Kind("federation_scoped_cursor"):
		record = &db.ScopedDeliveryCursor{}
		nullable = []string{}
		required = []string{"binding_uid", "epoch", "direction", "channel", "received_sequence", "received_digest", "accepted_sequence", "accepted_digest", "emitted_sequence", "emitted_digest", "acknowledged_sequence", "acknowledged_digest"}
	case Kind("federation_source_operation"):
		record = &db.ScopedSourceOperationExport{}
		nullable = []string{"admission_bytes", "terminal_bytes", "idempotency_key"}
		required = []string{"operation_uid", "digest", "source_instance_uid", "command_kind", "manifest_bytes", "principal_bytes", "principal_key", "state", "overlay_state", "admission_bytes", "terminal_bytes", "cancel_requested", "idempotency_key", "request_fingerprint"}
	case Kind("federation_private_source"):
		record = &db.ScopedPrivateSourceExport{}
		nullable = []string{"operation_uid", "ordinal", "conversion_uid", "provenance_bytes"}
		required = []string{"event_uid", "content_hash", "project_uid", "operation_uid", "ordinal", "conversion_uid", "event_bytes", "provenance_bytes"}
	case Kind("federation_source_custody"):
		record = &db.ScopedSourceCustodyExport{}
		nullable = []string{}
		required = []string{"operation_uid", "binding_uid", "epoch", "direction", "principal_bytes", "manifest_digest", "owns_complete", "result_acknowledged", "invalidation_state"}
	case Kind("federation_source_dependency"):
		record = &db.ScopedSourceDependencyExport{}
		nullable = []string{"establishing_operation_uid", "terminal_reason"}
		required = []string{"operation_uid", "project_uid", "issue_uid", "establishing_operation_uid", "proof_bytes", "state", "terminal_reason"}
	case Kind("federation_source_coverage"):
		record = &db.ScopedSourceCoverageExport{}
		nullable = []string{}
		required = []string{"operation_uid", "view_uid", "generation", "cut_digest", "outcome", "certificate_bytes"}
	case Kind("federation_control_custody"):
		record = &db.ScopedControlCustodyExport{}
		nullable = []string{"result_bytes", "conversion_uid"}
		required = []string{"request_uid", "binding_uid", "request_digest", "principal_bytes", "request_bytes", "result_bytes", "state", "result_acknowledged", "conversion_uid"}
	case Kind("federation_mutation_expectation"):
		record = &db.ScopedMutationExpectationExport{}
		nullable = []string{"consumed_operation_uid"}
		required = []string{"token_digest", "project_uid", "subject_uid", "predicate_kind", "predicate_bytes", "principal_bytes", "route_digest", "nonce", "expires_at", "consumed_operation_uid"}
	case Kind("federation_predicate_incarnation"):
		record = &db.ScopedPredicateIncarnationExport{}
		nullable = []string{}
		required = []string{"project_uid", "subject_uid", "predicate_kind", "incarnation", "canonical_digest"}
	case Kind("federation_canonical_close_state"):
		record = &db.ScopedCanonicalCloseExport{}
		nullable = []string{"status_intent_uid", "value_bytes"}
		required = []string{"issue_uid", "project_uid", "status_intent_uid", "status_clock_bytes", "value_bytes"}
	case Kind("federation_protocol_cutover"):
		record = &db.ScopedProtocolCutoverExport{}
		nullable = []string{}
		required = []string{"cutover_uid", "request_digest", "project_uids_bytes", "floor", "readiness_bytes", "suspended_bindings_bytes", "certificate_bytes", "state"}
	case Kind("federation_scoped_conversion"):
		record = &db.ScopedConversionExport{}
		nullable = []string{"root_cutover_uid", "inventory_digest", "baseline_digest", "result_bytes"}
		required = []string{"conversion_uid", "request_digest", "request_bytes", "old_bindings_bytes", "root_cutover_uid", "inventory_digest", "baseline_digest", "phase", "result_bytes"}
	case Kind("federation_conversion_obligation"):
		record = &db.ScopedConversionObligationExport{}
		nullable = []string{}
		required = []string{"conversion_uid", "obligation_uid", "kind", "binding_uid", "object_uid", "digest", "epoch", "sequence", "principal_key", "state"}
	default:
		return nil, false, nil
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(env.Data, &members); err != nil {
		return nil, true, err
	}
	for _, key := range required {
		value, ok := members[key]
		if !ok {
			return nil, true, fmt.Errorf("scoped row %s missing required member %s", env.Kind, key)
		}
		if !slices.Contains(nullable, key) && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, true, fmt.Errorf("scoped row %s forbids null for %s", env.Kind, key)
		}
	}
	if err := json.Unmarshal(env.Data, record, json.RejectUnknownMembers(true)); err != nil {
		return nil, true, err
	}

	return record, true, db.ValidateScopedImportRecord(record)
}
