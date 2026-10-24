package db

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
)

// ExportScopedStateSQL reads only this closed typed namespace, in dependency
// and primary-key order. Native callers supply their pinned snapshot reader.
func ExportScopedStateSQL(ctx context.Context, query func(context.Context, string, ...any) (*sql.Rows, error)) iter.Seq2[ImportRecord, error] {
	return func(yield func(ImportRecord, error) bool) {
		exportRows := func(table, statement string, scan func(*sql.Rows) (ImportRecord, error)) bool {
			rows, err := query(ctx, statement)
			if err != nil {
				yield(nil, fmt.Errorf("export %s: %w", table, err))
				return false
			}
			keep := true
			for rows.Next() {
				var record ImportRecord
				record, err = scan(rows)
				if err == nil {
					err = ValidateScopedImportRecord(record)
				}
				if err != nil {
					break
				}
				if !yield(record, nil) {
					keep = false
					break
				}
			}
			if err == nil {
				err = rows.Err()
			}
			_ = rows.Close()
			if !keep {
				return false
			}
			if err != nil {
				yield(nil, fmt.Errorf("export %s: %w", table, err))
				return false
			}
			return keep
		}
		if !exportRows("federation_scoped_views", `SELECT view_uid,catalog_entry_uid,authority_uid,binding_uid,epoch,accountable_actor,route_digest,destination_uid,active_generation,prepared_generation,state,reason_code,request_digest FROM federation_scoped_views ORDER BY view_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedViewExport
			err := rows.Scan(&r.ViewUID, &r.CatalogEntryUID, &r.AuthorityUID, &r.BindingUID, &r.Epoch, &r.AccountableActor, &r.RouteDigest, &r.DestinationUID, &r.ActiveGeneration, &r.PreparedGeneration, &r.State, &r.ReasonCode, &r.RequestDigest)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_scoped_generations", `SELECT view_uid,generation,descriptor_bytes,descriptor_digest,checkpoint_bytes,checkpoint_digest,cut_sequence,cut_digest,state,assembled_bytes,root_cutover_uid FROM federation_scoped_generations ORDER BY view_uid,generation`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedGenerationExport
			err := rows.Scan(&r.ViewUID, &r.Generation, &r.DescriptorBytes, &r.DescriptorDigest, &r.CheckpointBytes, &r.CheckpointDigest, &r.CutSequence, &r.CutDigest, &r.State, &r.AssembledBytes, &r.RootCutoverUID)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_scoped_projects", `SELECT project_uid,owner_view_uid,owner_generation,protocol_floor,root_cutover_uid,conversion_uid,serving_fenced FROM federation_scoped_projects ORDER BY project_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedProjectExport
			var serving_fenced int64
			err := rows.Scan(&r.ProjectUID, &r.OwnerViewUID, &r.OwnerGeneration, &r.ProtocolFloor, &r.RootCutoverUID, &r.ConversionUID, &serving_fenced)
			if err != nil {
				return nil, err
			}
			if serving_fenced != 0 && serving_fenced != 1 {
				err = fmt.Errorf("invalid scoped boolean serving_fenced")
				return nil, err
			}
			r.ServingFenced = serving_fenced == 1
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_canonical_close_state", `SELECT issue_uid,project_uid,status_intent_uid,status_clock_bytes,value_bytes FROM federation_canonical_close_state ORDER BY issue_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedCanonicalCloseExport
			err := rows.Scan(&r.IssueUID, &r.ProjectUID, &r.StatusIntentUID, &r.StatusClockBytes, &r.ValueBytes)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_predicate_incarnations", `SELECT project_uid,subject_uid,predicate_kind,incarnation,canonical_digest FROM federation_predicate_incarnations ORDER BY project_uid,subject_uid,predicate_kind`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedPredicateIncarnationExport
			err := rows.Scan(&r.ProjectUID, &r.SubjectUID, &r.PredicateKind, &r.Incarnation, &r.CanonicalDigest)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_mutation_expectations", `SELECT token_digest,project_uid,subject_uid,predicate_kind,predicate_bytes,principal_bytes,route_digest,nonce,expires_at,consumed_operation_uid FROM federation_mutation_expectations ORDER BY token_digest`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedMutationExpectationExport
			err := rows.Scan(&r.TokenDigest, &r.ProjectUID, &r.SubjectUID, &r.PredicateKind, &r.PredicateBytes, &r.PrincipalBytes, &r.RouteDigest, &r.Nonce, &r.ExpiresAt, &r.ConsumedOperationUID)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_protocol_cutovers", `SELECT cutover_uid,request_digest,project_uids_bytes,floor,readiness_bytes,suspended_bindings_bytes,certificate_bytes,state FROM federation_protocol_cutovers ORDER BY cutover_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedProtocolCutoverExport
			err := rows.Scan(&r.CutoverUID, &r.RequestDigest, &r.ProjectUIDsBytes, &r.Floor, &r.ReadinessBytes, &r.SuspendedBindingsBytes, &r.CertificateBytes, &r.State)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_scoped_conversions", `SELECT conversion_uid,request_digest,request_bytes,old_bindings_bytes,root_cutover_uid,inventory_digest,baseline_digest,phase,result_bytes FROM federation_scoped_conversions ORDER BY conversion_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedConversionExport
			err := rows.Scan(&r.ConversionUID, &r.RequestDigest, &r.RequestBytes, &r.OldBindingsBytes, &r.RootCutoverUID, &r.InventoryDigest, &r.BaselineDigest, &r.Phase, &r.ResultBytes)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_source_operations", `SELECT operation_uid,digest,source_instance_uid,command_kind,manifest_bytes,principal_bytes,principal_key,state,overlay_state,admission_bytes,terminal_bytes,cancel_requested,idempotency_key,request_fingerprint FROM federation_source_operations ORDER BY operation_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedSourceOperationExport
			var cancel_requested int64
			err := rows.Scan(&r.OperationUID, &r.Digest, &r.SourceInstanceUID, &r.CommandKind, &r.ManifestBytes, &r.PrincipalBytes, &r.PrincipalKey, &r.State, &r.OverlayState, &r.AdmissionBytes, &r.TerminalBytes, &cancel_requested, &r.IdempotencyKey, &r.RequestFingerprint)
			if err != nil {
				return nil, err
			}
			if cancel_requested != 0 && cancel_requested != 1 {
				err = fmt.Errorf("invalid scoped boolean cancel_requested")
				return nil, err
			}
			r.CancelRequested = cancel_requested == 1
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_private_sources", `SELECT event_uid,content_hash,project_uid,operation_uid,ordinal,conversion_uid,event_bytes,provenance_bytes FROM federation_private_sources ORDER BY event_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedPrivateSourceExport
			err := rows.Scan(&r.EventUID, &r.ContentHash, &r.ProjectUID, &r.OperationUID, &r.Ordinal, &r.ConversionUID, &r.EventBytes, &r.ProvenanceBytes)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_source_dependencies", `SELECT operation_uid,project_uid,issue_uid,establishing_operation_uid,proof_bytes,state,terminal_reason FROM federation_source_dependencies ORDER BY operation_uid,project_uid,issue_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedSourceDependencyExport
			err := rows.Scan(&r.OperationUID, &r.ProjectUID, &r.IssueUID, &r.EstablishingOperationUID, &r.ProofBytes, &r.State, &r.TerminalReason)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_source_custody", `SELECT operation_uid,binding_uid,epoch,direction,principal_bytes,manifest_digest,owns_complete,result_acknowledged,invalidation_state FROM federation_source_custody ORDER BY operation_uid,binding_uid,epoch,direction`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedSourceCustodyExport
			var owns_complete, result_acknowledged int64
			err := rows.Scan(&r.OperationUID, &r.BindingUID, &r.Epoch, &r.Direction, &r.PrincipalBytes, &r.ManifestDigest, &owns_complete, &result_acknowledged, &r.InvalidationState)
			if err != nil {
				return nil, err
			}
			if owns_complete != 0 && owns_complete != 1 {
				err = fmt.Errorf("invalid scoped boolean owns_complete")
				return nil, err
			}
			r.OwnsComplete = owns_complete == 1
			if result_acknowledged != 0 && result_acknowledged != 1 {
				err = fmt.Errorf("invalid scoped boolean result_acknowledged")
				return nil, err
			}
			r.ResultAcknowledged = result_acknowledged == 1
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_source_coverage", `SELECT operation_uid,view_uid,generation,cut_digest,outcome,certificate_bytes FROM federation_source_coverage ORDER BY operation_uid,view_uid,generation,cut_digest`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedSourceCoverageExport
			err := rows.Scan(&r.OperationUID, &r.ViewUID, &r.Generation, &r.CutDigest, &r.Outcome, &r.CertificateBytes)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_control_custody", `SELECT request_uid,binding_uid,request_digest,principal_bytes,request_bytes,result_bytes,state,result_acknowledged,conversion_uid FROM federation_control_custody ORDER BY request_uid,binding_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedControlCustodyExport
			var result_acknowledged int64
			err := rows.Scan(&r.RequestUID, &r.BindingUID, &r.RequestDigest, &r.PrincipalBytes, &r.RequestBytes, &r.ResultBytes, &r.State, &result_acknowledged, &r.ConversionUID)
			if err != nil {
				return nil, err
			}
			if result_acknowledged != 0 && result_acknowledged != 1 {
				err = fmt.Errorf("invalid scoped boolean result_acknowledged")
				return nil, err
			}
			r.ResultAcknowledged = result_acknowledged == 1
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_conversion_obligations", `SELECT conversion_uid,obligation_uid,kind,binding_uid,object_uid,digest,epoch,sequence,principal_key,state FROM federation_conversion_obligations ORDER BY conversion_uid,obligation_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedConversionObligationExport
			err := rows.Scan(&r.ConversionUID, &r.ObligationUID, &r.Kind, &r.BindingUID, &r.ObjectUID, &r.Digest, &r.Epoch, &r.Sequence, &r.PrincipalKey, &r.State)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_scoped_work", `SELECT view_uid,dirty_revision,captured_revision,capture_uid,capture_bytes,capture_digest,state FROM federation_scoped_work ORDER BY view_uid`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedWorkExport
			err := rows.Scan(&r.ViewUID, &r.DirtyRevision, &r.CapturedRevision, &r.CaptureUID, &r.CaptureBytes, &r.CaptureDigest, &r.State)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_scoped_records", `SELECT view_uid,generation,collection,record_key,project_uid,subject_uid,peer_project_uid,peer_uid,visible_revision,present,record_bytes,digest FROM federation_scoped_records ORDER BY view_uid,generation,collection,record_key`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedRecordExport
			var present int64
			err := rows.Scan(&r.ViewUID, &r.Generation, &r.Collection, &r.RecordKey, &r.ProjectUID, &r.SubjectUID, &r.PeerProjectUID, &r.PeerUID, &r.VisibleRevision, &present, &r.RecordBytes, &r.Digest)
			if err != nil {
				return nil, err
			}
			if present != 0 && present != 1 {
				err = fmt.Errorf("invalid scoped boolean present")
				return nil, err
			}
			r.Present = present == 1
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_scoped_batches", `SELECT view_uid,generation,sequence,digest,previous_digest,capture_uid,batch_bytes FROM federation_scoped_batches ORDER BY view_uid,generation,sequence`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedBatchExport
			err := rows.Scan(&r.ViewUID, &r.Generation, &r.Sequence, &r.Digest, &r.PreviousDigest, &r.CaptureUID, &r.BatchBytes)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_scoped_delivery", `SELECT binding_uid,epoch,direction,channel,sequence,object_uid,object_digest,offered_digest,header_bytes,envelope_bytes,accepted,emitted,acknowledged,upstream_binding_uid,upstream_epoch,upstream_direction,upstream_channel,upstream_sequence FROM federation_scoped_delivery ORDER BY binding_uid,epoch,direction,channel,sequence`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedDeliveryRecord
			var accepted, emitted, acknowledged int64
			var upstreamBinding, upstreamDirection, upstreamChannel sql.NullString
			var upstreamEpoch, upstreamSequence sql.NullInt64
			err := rows.Scan(&r.BindingUID, &r.Epoch, &r.Direction, &r.Channel, &r.Sequence, &r.ObjectUID, &r.ObjectDigest, &r.OfferedDigest, &r.HeaderBytes, &r.EnvelopeBytes, &accepted, &emitted, &acknowledged, &upstreamBinding, &upstreamEpoch, &upstreamDirection, &upstreamChannel, &upstreamSequence)
			if err != nil {
				return nil, err
			}
			if accepted != 0 && accepted != 1 {
				err = fmt.Errorf("invalid scoped boolean accepted")
				return nil, err
			}
			r.Accepted = accepted == 1
			if emitted != 0 && emitted != 1 {
				err = fmt.Errorf("invalid scoped boolean emitted")
				return nil, err
			}
			r.Emitted = emitted == 1
			if acknowledged != 0 && acknowledged != 1 {
				err = fmt.Errorf("invalid scoped boolean acknowledged")
				return nil, err
			}
			r.Acknowledged = acknowledged == 1
			if upstreamBinding.Valid || upstreamEpoch.Valid || upstreamDirection.Valid || upstreamChannel.Valid || upstreamSequence.Valid {
				if !(upstreamBinding.Valid && upstreamEpoch.Valid && upstreamDirection.Valid && upstreamChannel.Valid && upstreamSequence.Valid) {
					err = fmt.Errorf("partial upstream scoped delivery tuple")
					return nil, err
				}
				r.Upstream = &ScopedDeliveryRef{ScopedDeliveryStream: ScopedDeliveryStream{BindingUID: upstreamBinding.String, Epoch: upstreamEpoch.Int64, Direction: ScopedDeliveryDirection(upstreamDirection.String), Channel: ScopedDeliveryChannel(upstreamChannel.String)}, Sequence: upstreamSequence.Int64}
			}
			return &r, nil
		}) {
			return
		}
		if !exportRows("federation_scoped_cursors", `SELECT binding_uid,epoch,direction,channel,received_sequence,received_digest,accepted_sequence,accepted_digest,emitted_sequence,emitted_digest,acknowledged_sequence,acknowledged_digest FROM federation_scoped_cursors ORDER BY binding_uid,epoch,direction,channel`, func(rows *sql.Rows) (ImportRecord, error) {
			var r ScopedDeliveryCursor
			err := rows.Scan(&r.BindingUID, &r.Epoch, &r.Direction, &r.Channel, &r.ReceivedSequence, &r.ReceivedDigest, &r.AcceptedSequence, &r.AcceptedDigest, &r.EmittedSequence, &r.EmittedDigest, &r.AcknowledgedSequence, &r.AcknowledgedDigest)
			if err != nil {
				return nil, err
			}
			return &r, nil
		}) {
			return
		}
	}
}

// ReplayScopedRecord writes exact retained bytes inside the native replay
// transaction, after complete-graph validation and before commit.
func ReplayScopedRecord(ctx context.Context, tx *sql.Tx, record ImportRecord) error {
	if err := ValidateScopedImportRecord(record); err != nil {
		return err
	}
	switch r := record.(type) {
	case *ScopedViewExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_views(view_uid,catalog_entry_uid,authority_uid,binding_uid,epoch,accountable_actor,route_digest,destination_uid,active_generation,prepared_generation,state,reason_code,request_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, r.ViewUID, r.CatalogEntryUID, r.AuthorityUID, r.BindingUID, r.Epoch, r.AccountableActor, r.RouteDigest, r.DestinationUID, r.ActiveGeneration, r.PreparedGeneration, r.State, r.ReasonCode, r.RequestDigest)
		return err
	case *ScopedGenerationExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_generations(view_uid,generation,descriptor_bytes,descriptor_digest,checkpoint_bytes,checkpoint_digest,cut_sequence,cut_digest,state,assembled_bytes,root_cutover_uid) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, r.ViewUID, r.Generation, r.DescriptorBytes, r.DescriptorDigest, r.CheckpointBytes, r.CheckpointDigest, r.CutSequence, r.CutDigest, r.State, r.AssembledBytes, r.RootCutoverUID)
		return err
	case *ScopedProjectExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_projects(project_uid,owner_view_uid,owner_generation,protocol_floor,root_cutover_uid,conversion_uid,serving_fenced) VALUES($1,$2,$3,$4,$5,$6,$7)`, r.ProjectUID, r.OwnerViewUID, r.OwnerGeneration, r.ProtocolFloor, r.RootCutoverUID, r.ConversionUID, scopedSQLBool(r.ServingFenced))
		return err
	case *ScopedRecordExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_records(view_uid,generation,collection,record_key,project_uid,subject_uid,peer_project_uid,peer_uid,visible_revision,present,record_bytes,digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, r.ViewUID, r.Generation, r.Collection, r.RecordKey, r.ProjectUID, r.SubjectUID, r.PeerProjectUID, r.PeerUID, r.VisibleRevision, scopedSQLBool(r.Present), r.RecordBytes, r.Digest)
		return err
	case *ScopedBatchExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_batches(view_uid,generation,sequence,digest,previous_digest,capture_uid,batch_bytes) VALUES($1,$2,$3,$4,$5,$6,$7)`, r.ViewUID, r.Generation, r.Sequence, r.Digest, r.PreviousDigest, r.CaptureUID, r.BatchBytes)
		return err
	case *ScopedWorkExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_work(view_uid,dirty_revision,captured_revision,capture_uid,capture_bytes,capture_digest,state) VALUES($1,$2,$3,$4,$5,$6,$7)`, r.ViewUID, r.DirtyRevision, r.CapturedRevision, r.CaptureUID, r.CaptureBytes, r.CaptureDigest, r.State)
		return err
	case *ScopedDeliveryRecord:
		var upstreamBinding, upstreamEpoch, upstreamDirection, upstreamChannel, upstreamSequence any
		if r.Upstream != nil {
			upstreamBinding = r.Upstream.BindingUID
			upstreamEpoch = r.Upstream.Epoch
			upstreamDirection = string(r.Upstream.Direction)
			upstreamChannel = string(r.Upstream.Channel)
			upstreamSequence = r.Upstream.Sequence
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_delivery(binding_uid,epoch,direction,channel,sequence,object_uid,object_digest,offered_digest,header_bytes,envelope_bytes,accepted,emitted,acknowledged,upstream_binding_uid,upstream_epoch,upstream_direction,upstream_channel,upstream_sequence) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, r.BindingUID, r.Epoch, r.Direction, r.Channel, r.Sequence, r.ObjectUID, r.ObjectDigest, r.OfferedDigest, r.HeaderBytes, r.EnvelopeBytes, scopedSQLBool(r.Accepted), scopedSQLBool(r.Emitted), scopedSQLBool(r.Acknowledged), upstreamBinding, upstreamEpoch, upstreamDirection, upstreamChannel, upstreamSequence)
		return err
	case *ScopedDeliveryCursor:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_cursors(binding_uid,epoch,direction,channel,received_sequence,received_digest,accepted_sequence,accepted_digest,emitted_sequence,emitted_digest,acknowledged_sequence,acknowledged_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, r.BindingUID, r.Epoch, r.Direction, r.Channel, r.ReceivedSequence, r.ReceivedDigest, r.AcceptedSequence, r.AcceptedDigest, r.EmittedSequence, r.EmittedDigest, r.AcknowledgedSequence, r.AcknowledgedDigest)
		return err
	case *ScopedSourceOperationExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_source_operations(operation_uid,digest,source_instance_uid,command_kind,manifest_bytes,principal_bytes,principal_key,state,overlay_state,admission_bytes,terminal_bytes,cancel_requested,idempotency_key,request_fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, r.OperationUID, r.Digest, r.SourceInstanceUID, r.CommandKind, r.ManifestBytes, r.PrincipalBytes, r.PrincipalKey, r.State, r.OverlayState, r.AdmissionBytes, r.TerminalBytes, scopedSQLBool(r.CancelRequested), r.IdempotencyKey, r.RequestFingerprint)
		return err
	case *ScopedPrivateSourceExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_private_sources(event_uid,content_hash,project_uid,operation_uid,ordinal,conversion_uid,event_bytes,provenance_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, r.EventUID, r.ContentHash, r.ProjectUID, r.OperationUID, r.Ordinal, r.ConversionUID, r.EventBytes, r.ProvenanceBytes)
		return err
	case *ScopedSourceCustodyExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_source_custody(operation_uid,binding_uid,epoch,direction,principal_bytes,manifest_digest,owns_complete,result_acknowledged,invalidation_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, r.OperationUID, r.BindingUID, r.Epoch, r.Direction, r.PrincipalBytes, r.ManifestDigest, scopedSQLBool(r.OwnsComplete), scopedSQLBool(r.ResultAcknowledged), r.InvalidationState)
		return err
	case *ScopedSourceDependencyExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_source_dependencies(operation_uid,project_uid,issue_uid,establishing_operation_uid,proof_bytes,state,terminal_reason) VALUES($1,$2,$3,$4,$5,$6,$7)`, r.OperationUID, r.ProjectUID, r.IssueUID, r.EstablishingOperationUID, r.ProofBytes, r.State, r.TerminalReason)
		return err
	case *ScopedSourceCoverageExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_source_coverage(operation_uid,view_uid,generation,cut_digest,outcome,certificate_bytes) VALUES($1,$2,$3,$4,$5,$6)`, r.OperationUID, r.ViewUID, r.Generation, r.CutDigest, r.Outcome, r.CertificateBytes)
		return err
	case *ScopedControlCustodyExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_control_custody(request_uid,binding_uid,request_digest,principal_bytes,request_bytes,result_bytes,state,result_acknowledged,conversion_uid) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, r.RequestUID, r.BindingUID, r.RequestDigest, r.PrincipalBytes, r.RequestBytes, r.ResultBytes, r.State, scopedSQLBool(r.ResultAcknowledged), r.ConversionUID)
		return err
	case *ScopedMutationExpectationExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_mutation_expectations(token_digest,project_uid,subject_uid,predicate_kind,predicate_bytes,principal_bytes,route_digest,nonce,expires_at,consumed_operation_uid) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, r.TokenDigest, r.ProjectUID, r.SubjectUID, r.PredicateKind, r.PredicateBytes, r.PrincipalBytes, r.RouteDigest, r.Nonce, r.ExpiresAt, r.ConsumedOperationUID)
		return err
	case *ScopedPredicateIncarnationExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_predicate_incarnations(project_uid,subject_uid,predicate_kind,incarnation,canonical_digest) VALUES($1,$2,$3,$4,$5)`, r.ProjectUID, r.SubjectUID, r.PredicateKind, r.Incarnation, r.CanonicalDigest)
		return err
	case *ScopedCanonicalCloseExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_canonical_close_state(issue_uid,project_uid,status_intent_uid,status_clock_bytes,value_bytes) VALUES($1,$2,$3,$4,$5)`, r.IssueUID, r.ProjectUID, r.StatusIntentUID, r.StatusClockBytes, r.ValueBytes)
		return err
	case *ScopedProtocolCutoverExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_protocol_cutovers(cutover_uid,request_digest,project_uids_bytes,floor,readiness_bytes,suspended_bindings_bytes,certificate_bytes,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, r.CutoverUID, r.RequestDigest, r.ProjectUIDsBytes, r.Floor, r.ReadinessBytes, r.SuspendedBindingsBytes, r.CertificateBytes, r.State)
		return err
	case *ScopedConversionExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_scoped_conversions(conversion_uid,request_digest,request_bytes,old_bindings_bytes,root_cutover_uid,inventory_digest,baseline_digest,phase,result_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, r.ConversionUID, r.RequestDigest, r.RequestBytes, r.OldBindingsBytes, r.RootCutoverUID, r.InventoryDigest, r.BaselineDigest, r.Phase, r.ResultBytes)
		return err
	case *ScopedConversionObligationExport:
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_conversion_obligations(conversion_uid,obligation_uid,kind,binding_uid,object_uid,digest,epoch,sequence,principal_key,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, r.ConversionUID, r.ObligationUID, r.Kind, r.BindingUID, r.ObjectUID, r.Digest, r.Epoch, r.Sequence, r.PrincipalKey, r.State)
		return err
	default:
		return fmt.Errorf("unknown scoped replay record %T", record)
	}
}
func scopedSQLBool(value bool) int {
	if value {
		return 1
	}
	return 0
}
