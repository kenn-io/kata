package db

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"go.kenn.io/kata/internal/uid"
)

func scopedIDs(ids ...string) bool {
	for _, id := range ids {
		if !uid.Valid(id) {
			return false
		}
	}
	return true
}
func scopedOptionalUID(id *string) bool                { return id == nil || uid.Valid(*id) }
func scopedState(value string, allowed ...string) bool { return slices.Contains(allowed, value) }
func scopedSubject(p ScopedMutationPreparation) string {
	switch p.Subject.Kind {
	case "project":
		return p.Subject.ProjectUID
	case "issue":
		return p.Subject.IssueUID
	default:
		return p.Subject.DefinitionUID
	}
}
func scopedJSON(raw []byte, out any) error {
	if len(raw) == 0 || len(raw) > MaxScopedEnvelopeBytes {
		return errors.New("missing/oversized retained scoped body")
	}
	return DecodeScopedRecord(raw, out)
}

// ValidateScopedImportRecord validates the closed physical row shape. Expected
// signed context and relationships are checked by the complete graph below;
// this function never treats embedded keys or unverified bodies as authority.
func ValidateScopedImportRecord(record ImportRecord) error {
	if !IsScopedImportRecord(record) || reflect.ValueOf(record).IsNil() {
		return fmt.Errorf("unknown/nil scoped import record %T", record)
	}
	invalid := func() error { return fmt.Errorf("invalid %s row", record.ImportKind()) }
	switch r := record.(type) {
	case *ScopedViewExport:
		principal := CustodyPrincipal{AuthorityUID: r.AuthorityUID, BindingUID: r.BindingUID, Epoch: r.Epoch, AccountableActor: r.AccountableActor, RouteDigest: r.RouteDigest}
		if !scopedIDs(r.ViewUID, r.CatalogEntryUID, r.DestinationUID) || principal.Validate() != nil || !scopedDigest(r.RequestDigest) || !scopedState(r.State, "active", "prepared", "fenced", "retired") {
			return invalid()
		}
		if r.ActiveGeneration != nil && *r.ActiveGeneration <= 0 || r.PreparedGeneration != nil && *r.PreparedGeneration <= 0 || r.ActiveGeneration != nil && r.PreparedGeneration != nil && *r.ActiveGeneration == *r.PreparedGeneration {
			return invalid()
		}
	case *ScopedGenerationExport:
		if !uid.Valid(r.ViewUID) || r.Generation <= 0 || !scopedDigest(r.DescriptorDigest) || ScopedOfferedDigest(r.DescriptorBytes) != r.DescriptorDigest || r.AssembledBytes < 0 || r.AssembledBytes > MaxScopedCheckpointBytes || !uid.Valid(r.RootCutoverUID) || !scopedState(r.State, "active", "prepared", "retired", "fenced") {
			return invalid()
		}
		var view ScopedViewDescriptor
		if err := scopedJSON(r.DescriptorBytes, &view); err != nil {
			return err
		}
		if view.Key != (ScopedViewKey{r.ViewUID, r.Generation}) || view.RootCutoverUID != r.RootCutoverUID {
			return invalid()
		}
		if r.CheckpointBytes == nil {
			if r.CheckpointDigest != nil || r.CutSequence != 0 || r.CutDigest != "" || r.AssembledBytes != 0 || r.State == "active" {
				return invalid()
			}
		} else {
			var c ScopedCheckpoint
			if err := scopedJSON(r.CheckpointBytes, &c); err != nil {
				return err
			}
			if len(r.CheckpointBytes) > MaxScopedCheckpointBytes || r.CheckpointDigest == nil || *r.CheckpointDigest != c.Cut.Digest || r.CutSequence != c.Cut.Sequence || r.CutDigest != c.Cut.Digest || r.AssembledBytes != int64(len(r.CheckpointBytes)) || !reflect.DeepEqual(view, c.View) {
				return invalid()
			}
		}
	case *ScopedProjectExport:
		if !uid.Valid(r.ProjectUID) || !scopedOptionalUID(r.OwnerViewUID) || !scopedOptionalUID(r.RootCutoverUID) || !scopedOptionalUID(r.ConversionUID) || (r.OwnerViewUID == nil) != (r.OwnerGeneration == nil) || r.OwnerGeneration != nil && *r.OwnerGeneration <= 0 || r.ProtocolFloor < 1 || r.ProtocolFloor > ScopedRelayProtocolVersion || r.ProtocolFloor == ScopedRelayProtocolVersion && r.RootCutoverUID == nil {
			return invalid()
		}
	case *ScopedRecordExport:
		return ValidateScopedRecordEnvelope(*r)
	case *ScopedBatchExport:
		if !uid.Valid(r.ViewUID) || r.Generation <= 0 || r.Sequence <= 0 || !scopedDigest(r.Digest) || !scopedDigest(r.PreviousDigest) || !uid.Valid(r.CaptureUID) {
			return invalid()
		}
		var batch ScopedBatch
		if err := scopedJSON(r.BatchBytes, &batch); err != nil {
			return err
		}
		if len(r.BatchBytes) > MaxScopedCheckpointBytes || batch.View.Key != (ScopedViewKey{r.ViewUID, r.Generation}) || batch.Cut.Sequence != r.Sequence || batch.Cut.Digest != r.Digest || batch.Cut.CaptureUID != r.CaptureUID || batch.PreviousDigest != r.PreviousDigest {
			return invalid()
		}
	case *ScopedWorkExport:
		if !uid.Valid(r.ViewUID) || r.DirtyRevision < 0 || !scopedState(r.State, "idle", "dirty", "captured") {
			return invalid()
		}
		captured := r.CapturedRevision != nil || r.CaptureUID != nil || r.CaptureBytes != nil || r.CaptureDigest != nil
		if captured {
			if r.CapturedRevision == nil || r.CaptureUID == nil || r.CaptureBytes == nil || r.CaptureDigest == nil || *r.CapturedRevision < 0 || *r.CapturedRevision > r.DirtyRevision || !uid.Valid(*r.CaptureUID) || !scopedDigest(*r.CaptureDigest) || ScopedOfferedDigest(r.CaptureBytes) != *r.CaptureDigest || r.State != "captured" {
				return invalid()
			}
		} else if r.State == "captured" {
			return invalid()
		}
	case *ScopedDeliveryRecord:
		return r.Validate()
	case *ScopedDeliveryCursor:
		return r.Validate()
	case *ScopedSourceOperationExport:
		submission, err := DecodeScopedSourceSubmission(r.ManifestBytes)
		if err != nil {
			return err
		}
		op := submission.Operation
		var principal CustodyPrincipal
		if err := scopedJSON(r.PrincipalBytes, &principal); err != nil {
			return err
		}
		key, err := ScopedPrincipalKey(principal)
		if err != nil {
			return err
		}
		if r.OperationUID != op.UID || r.Digest != op.Digest || r.SourceInstanceUID != op.SourceInstanceUID || r.CommandKind != op.CommandKind || principal != op.Principal || key != r.PrincipalKey || scopedPtrValue(r.IdempotencyKey) != op.IdempotencyKey || !scopedDigest(r.RequestFingerprint) || !scopedState(r.State, "staged", "accepted", "rejected", "canceled") || !scopedState(r.OverlayState, "pending", "conflict", "settled", "accepted_private", "none") {
			return invalid()
		}
		if r.AdmissionBytes != nil {
			var evidence ScopedAdmissionEvidence
			if err := scopedJSON(r.AdmissionBytes, &evidence); err != nil {
				return err
			}
			if evidence.OperationUID != op.UID || evidence.OperationDigest != op.Digest || evidence.Principal != principal {
				return invalid()
			}
		}
		if r.TerminalBytes != nil {
			result, err := DecodeScopedSourceTerminalResult(r.TerminalBytes)
			if err != nil {
				return err
			}
			if result.OperationUID != op.UID || result.OperationDigest != op.Digest || result.State != r.State {
				return invalid()
			}
		} else if r.State != "staged" {
			return invalid()
		}
	case *ScopedPrivateSourceExport:
		event, err := DecodeRelaySourceEvent(r.EventBytes)
		if err != nil {
			return err
		}
		if event.EventUID != r.EventUID || event.ContentHash != r.ContentHash || event.ProjectUID != r.ProjectUID || !scopedOptionalUID(r.OperationUID) || !scopedOptionalUID(r.ConversionUID) || (r.OperationUID == nil) != (r.Ordinal == nil) || r.Ordinal != nil && *r.Ordinal < 0 {
			return invalid()
		}
		if r.ProvenanceBytes != nil {
			var receipt AttributionReceipt
			if err := scopedJSON(r.ProvenanceBytes, &receipt); err != nil {
				return err
			}
			if _, err := receiptSigningBytes(receipt); err != nil {
				return err
			}
			if len(receipt.Signature) != ed25519.SignatureSize || receipt.ProjectUID != r.ProjectUID || receipt.EventUID != r.EventUID || receipt.ContentHash != r.ContentHash || receipt.SourceActor != event.Actor {
				return invalid()
			}
			teammate, _, err := EventCreationProvenance(event)
			if err != nil || teammate != receipt.Teammate {
				return invalid()
			}
		}
	case *ScopedSourceCustodyExport:
		var principal CustodyPrincipal
		if err := scopedJSON(r.PrincipalBytes, &principal); err != nil {
			return err
		}
		if !uid.Valid(r.OperationUID) || principal.BindingUID != r.BindingUID || principal.Epoch != r.Epoch || !scopedDigest(r.ManifestDigest) || !scopedState(r.Direction, "inbound", "outbound") || !scopedState(r.InvalidationState, "none", "requested", "effective") {
			return invalid()
		}
	case *ScopedSourceDependencyExport:
		if !scopedIDs(r.OperationUID, r.ProjectUID, r.IssueUID) || !scopedOptionalUID(r.EstablishingOperationUID) || !scopedState(r.State, "pending", "satisfied", "rejected", "canceled") {
			return invalid()
		}
		var proof ScopedMembershipProof
		if err := scopedJSON(r.ProofBytes, &proof); err != nil {
			return err
		}
		if proof.Endpoint != (ScopedEndpoint{r.ProjectUID, r.IssueUID}) || proof.Kind == "root_membership" && (r.EstablishingOperationUID != nil || proof.CreatingOperationUID != "") || proof.Kind == "component_creation" && (r.EstablishingOperationUID == nil || proof.CreatingOperationUID != *r.EstablishingOperationUID) || !scopedState(proof.Kind, "root_membership", "component_creation") || (r.State == "rejected" || r.State == "canceled") && (r.TerminalReason == nil || *r.TerminalReason == "") {
			return invalid()
		}
	case *ScopedSourceCoverageExport:
		var coverage SourceCoverage
		if err := scopedJSON(r.CertificateBytes, &coverage); err != nil {
			return err
		}
		if _, err := ScopedSourceCoverageObjectDigest(coverage); err != nil {
			return err
		}
		if coverage.OperationUID != r.OperationUID || coverage.View != (ScopedViewKey{r.ViewUID, r.Generation}) || coverage.Cut.Digest != r.CutDigest || coverage.Outcome != r.Outcome {
			return invalid()
		}
	case *ScopedControlCustodyExport:
		var principal CustodyPrincipal
		if err := scopedJSON(r.PrincipalBytes, &principal); err != nil {
			return err
		}
		var request ScopedControlRequest
		if err := scopedJSON(r.RequestBytes, &request); err != nil {
			return err
		}
		if principal.BindingUID != r.BindingUID || request.RequestUID != r.RequestUID || request.Digest != r.RequestDigest || !scopedOptionalUID(r.ConversionUID) || !scopedState(r.State, "pending", "accepted", "rejected") {
			return invalid()
		}
		if r.ResultBytes != nil {
			var result ScopedControlResult
			if err := scopedJSON(r.ResultBytes, &result); err != nil {
				return err
			}
			if result.RequestUID != r.RequestUID || result.Digest != r.RequestDigest || result.State != r.State {
				return invalid()
			}
		} else if r.ResultAcknowledged || r.State != "pending" {
			return invalid()
		}
	case *ScopedMutationExpectationExport:
		var principal CustodyPrincipal
		if err := scopedJSON(r.PrincipalBytes, &principal); err != nil {
			return err
		}
		var predicate ScopedMutationPredicate
		if err := scopedJSON(r.PredicateBytes, &predicate); err != nil {
			return err
		}
		storageKind, err := ScopedPredicateStorageKind(predicate.Preparation)
		if err != nil {
			return err
		}
		expires, err := time.Parse(time.RFC3339Nano, r.ExpiresAt)
		if err != nil || expires.IsZero() || !scopedIDs(r.ProjectUID, r.SubjectUID) || !scopedDigest(r.TokenDigest) || r.Nonce == "" || len(r.Nonce) > 256 || principal.RouteDigest != r.RouteDigest || predicate.Preparation.Subject.ProjectUID != r.ProjectUID || scopedSubject(predicate.Preparation) != r.SubjectUID || storageKind != r.PredicateKind || !scopedOptionalUID(r.ConsumedOperationUID) {
			return invalid()
		}
	case *ScopedPredicateIncarnationExport:
		if !scopedIDs(r.ProjectUID, r.SubjectUID) || r.Incarnation <= 0 || !scopedDigest(r.CanonicalDigest) || !scopedState(r.PredicateKind, "project.metadata", "issue.metadata", "issue.parent", "issue.close", "job.definition", "workflow.definition") {
			return invalid()
		}
	case *ScopedCanonicalCloseExport:
		if !scopedIDs(r.IssueUID, r.ProjectUID) || !scopedOptionalUID(r.StatusIntentUID) {
			return invalid()
		}
		if _, err := scopedCanonicalCloseRow(r); err != nil {
			return err
		}
	case *ScopedProtocolCutoverExport:
		var projects []string
		if err := scopedJSON(r.ProjectUIDsBytes, &projects); err != nil {
			return err
		}
		var certificate ScopedProtocolCutoverCertificate
		if err := scopedJSON(r.CertificateBytes, &certificate); err != nil {
			return err
		}
		cut := certificate.Cutover
		if r.CutoverUID != cut.UID || r.RequestDigest != cut.RequestDigest || int64(cut.Floor) != r.Floor || !slices.Equal(projects, cut.ProjectUIDs) || ScopedOfferedDigest(r.ReadinessBytes) != cut.ReadinessDigest || ScopedOfferedDigest(r.SuspendedBindingsBytes) != cut.SuspendedBindingsDigest || len(r.ReadinessBytes) == 0 || len(r.SuspendedBindingsBytes) == 0 || !scopedState(r.State, "committed", "retired") {
			return invalid()
		}
	case *ScopedConversionExport:
		var request ScopedConversionRequest
		if err := scopedJSON(r.RequestBytes, &request); err != nil {
			return err
		}
		var old []ScopedOldBinding
		if err := scopedJSON(r.OldBindingsBytes, &old); err != nil {
			return err
		}
		state := ScopedConversionState{Request: request, Phase: r.Phase, RootCutoverUID: scopedPtrValue(r.RootCutoverUID), InventoryDigest: scopedPtrValue(r.InventoryDigest), BaselineDigest: scopedPtrValue(r.BaselineDigest), OldBindings: old}
		if err := state.Validate(); err != nil {
			return err
		}
		if request.UID != r.ConversionUID || request.Digest != r.RequestDigest {
			return invalid()
		}
		if r.ResultBytes != nil {
			var result ScopedConversionState
			if err := scopedJSON(r.ResultBytes, &result); err != nil {
				return err
			}
			if !reflect.DeepEqual(result, state) {
				return invalid()
			}
		}
	case *ScopedConversionObligationExport:
		if !scopedIDs(r.ConversionUID, r.ObligationUID, r.BindingUID, r.ObjectUID) || !scopedDigest(r.Digest) || !scopedDigest(r.PrincipalKey) || r.Epoch <= 0 || r.Sequence < 0 || !scopedState(r.Kind, "delivery", "source", "result", "coverage", "control", "checkpoint", "capture", "conversion") || !scopedState(r.State, "pending", "settled") {
			return invalid()
		}
	}
	return nil
}
func scopedPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// Native source codecs are the sole retained-body decoder. Equality here is
// exact byte ownership, not decode/remarshal equivalence.
func scopedSameOriginal(row *ScopedPrivateSourceExport, submission SourceSubmission, ordinal int) bool {
	if ordinal < 0 || ordinal >= len(submission.OriginalBytes) {
		return false
	}
	member := submission.Operation.Members[ordinal]
	return row.EventUID == member.EventUID && row.ProjectUID == member.ProjectUID && row.ContentHash == member.ContentHash && bytes.Equal(row.EventBytes, submission.OriginalBytes[ordinal])
}

// Decode a copy of retained clock/value bytes; the native row remains exact.
// Status-dependent semantics are checked with the indexed native issue below.
func scopedCanonicalCloseRow(row *ScopedCanonicalCloseExport) (CanonicalCloseState, error) {
	clock, err := DecodeScopedCloseClockProof(row.StatusClockBytes)
	if err != nil {
		return CanonicalCloseState{}, err
	}
	state := CanonicalCloseState{IssueUID: row.IssueUID, StatusIntentUID: scopedPtrValue(row.StatusIntentUID), StatusClock: clock.StatusClock, CloseBoundaryClock: clock.CloseBoundaryClock}
	if row.ValueBytes != nil {
		var value ScopedCloseValue
		if err := scopedJSON(row.ValueBytes, &value); err != nil {
			return CanonicalCloseState{}, err
		}
		state.Value = &value
	}
	return state, nil
}
