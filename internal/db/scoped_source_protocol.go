package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/kata/internal/uid"
)

var ErrScopedSourceConflict = errors.New("scoped source conflict")

var ErrScopedSourcePending = errors.New("scoped source component is pending")

type ScopedSourceAccess struct {
	View      ScopedViewKey
	Principal CustodyPrincipal
}

type scopedSourceAccessKey struct{}

func WithScopedSourceAccess(ctx context.Context, access ScopedSourceAccess) context.Context {
	return context.WithValue(ctx, scopedSourceAccessKey{}, access)
}

func ScopedSourceAccessFromContext(ctx context.Context) (ScopedSourceAccess, bool) {
	access, ok := ctx.Value(scopedSourceAccessKey{}).(ScopedSourceAccess)
	if !ok || access.View.Validate() != nil || access.Principal.Validate() != nil {
		return ScopedSourceAccess{}, false
	}
	return access, true
}

func (ScopedSourceAccess) MarshalJSON() ([]byte, error) {
	return nil, errors.New("ScopedSourceAccess is in-process only")
}

func (*ScopedSourceAccess) UnmarshalJSON([]byte) error {
	return errors.New("ScopedSourceAccess is not a wire type")
}

func scopedSourceManifest(op SourceOperation) error {
	if validateSourceOperationVersion(op) != nil || !uid.Valid(op.UID) || !uid.Valid(op.SourceInstanceUID) || SourceCommandClass(op.CommandKind).Disposition != "source" || op.Version != 3 && len(op.Members) == 0 {
		return errors.New("invalid scoped source manifest")
	}
	if err := op.Principal.Validate(); err != nil {
		return err
	}
	digest, err := SourceOperationDigest(op)
	if err != nil || digest != op.Digest {
		return ErrScopedSourceConflict
	}
	seen := map[string]bool{}
	for n, member := range op.Members {
		if !uid.Valid(member.ProjectUID) || !uid.Valid(member.EventUID) || !scopedDigest(member.ContentHash) || member.Ordinal != n || seen[member.EventUID] {
			return errors.New("invalid scoped source member")
		}
		seen[member.EventUID] = true
	}
	for _, token := range op.Expectations {
		if err := token.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func scopedSourceStatusTx(ctx context.Context, tx *sql.Tx, operation string) (SourceStatus, []byte, error) {
	status := SourceStatus{OperationUID: operation}
	var manifest, terminal []byte
	err := tx.QueryRowContext(ctx, `SELECT state,overlay_state,manifest_bytes,terminal_bytes FROM federation_source_operations WHERE operation_uid=$1`, operation).Scan(&status.State, &status.Settlement, &manifest, &terminal)
	if err != nil {
		return status, nil, err
	}
	if terminal != nil {
		result, err := DecodeScopedSourceTerminalResult(terminal)
		if err != nil {
			return status, nil, err
		}
		status.Result = &result
	}
	return status, manifest, nil
}

func scopedSourcePins(view ScopedViewDescriptor) []RootKeyPin {
	pins := make([]RootKeyPin, 0, len(view.Projects))
	for _, selected := range view.Projects {
		pins = append(pins, selected.Pin)
	}
	return pins
}

func scopedSourceCreation(op SourceSubmission, proof ScopedMembershipProof) bool {
	for _, event := range op.Originals {
		if event.Type != "issue.created" {
			continue
		}
		_, created, err := EventCreationProvenance(event)
		if err != nil {
			continue
		}
		for _, entity := range created {
			if entity.Kind == "issue" && entity.ProjectUID == proof.Endpoint.ProjectUID && entity.EntityUID == proof.Endpoint.IssueUID {
				return true
			}
		}
	}
	return false
}

func RetainScopedSourceTx(ctx context.Context, tx *sql.Tx, view ScopedViewDescriptor, receiving CustodyPrincipal, onward *CustodyPrincipal, submission SourceSubmission, referenceAliases ...func(string, string) (ScopedEndpoint, error)) (SourceStatus, error) {
	status, _, err := retainScopedSourceDataTx(ctx, tx, view, submission, referenceAliases...)
	if err != nil {
		return SourceStatus{}, err
	}
	if len(submission.Originals) < len(submission.Operation.Members) {
		return status, nil
	}
	op := submission.Operation
	if err := scopedRetainCustodyTx(ctx, tx, view, op, receiving, "inbound"); err != nil {
		return SourceStatus{}, err
	}
	if onward != nil {
		if err := scopedRetainCustodyTx(ctx, tx, view, op, *onward, "outbound"); err != nil {
			return SourceStatus{}, err
		}
	}
	return status, ScopedSourceBudgetTx(ctx, tx)
}

type scopedSourceRetentionPlan struct {
	status       SourceStatus
	born         bool
	operation    ScopedSourceOperationExport
	originals    []ScopedPrivateSourceExport
	dependencies []ScopedSourceDependencyExport
}

func prepareScopedSourceRetentionTx(ctx context.Context, tx *sql.Tx, view ScopedViewDescriptor, submission SourceSubmission, referenceAliases ...func(string, string) (ScopedEndpoint, error)) (scopedSourceRetentionPlan, error) {
	op := submission.Operation
	status := SourceStatus{OperationUID: op.UID, State: "staged", Settlement: "pending"}
	if err := scopedSourceManifest(op); err != nil {
		return scopedSourceRetentionPlan{}, err
	}
	if op.Principal.AuthorityUID != view.AuthorityUID || op.Principal.RouteDigest != view.RouteDigest || !scopedDescriptorHasHop(view, op.Principal.BindingUID, op.Principal.Epoch, op.SourceInstanceUID) {
		return scopedSourceRetentionPlan{}, ErrNotFound
	}
	selected := map[string]bool{}
	for _, pin := range view.Projects {
		selected[pin.ProjectUID] = true
	}
	// Only the closed, fully validated V3 arm owes input rather than producer
	// originals. Its stable project must still be selected by this exact view.
	if op.Version == 3 && !selected[op.ImportIntent.ProjectUID] {
		return scopedSourceRetentionPlan{}, ErrNotFound
	}
	for _, member := range op.Members {
		if !selected[member.ProjectUID] {
			return scopedSourceRetentionPlan{}, ErrNotFound
		}
	}
	if len(submission.Originals) > len(op.Members) || len(submission.OriginalBytes) != 0 && len(submission.OriginalBytes) != len(submission.Originals) {
		return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
	}
	// A partial request is not admitted as a logical operation. Inbound delivery
	// staging belongs to the transport; this method never ACKs a body prefix.
	if len(submission.Originals) < len(op.Members) {
		if submission.WireBytes != nil {
			return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
		}
		for n, event := range submission.Originals {
			member := op.Members[n]
			if event.EventUID != member.EventUID || event.ProjectUID != member.ProjectUID || event.ContentHash != member.ContentHash || event.OriginInstanceUID != op.SourceInstanceUID {
				return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
			}
			if _, _, err := ValidateRemoteEventContentHash(event); err != nil {
				return scopedSourceRetentionPlan{}, err
			}
			if len(submission.OriginalBytes) > 0 {
				decoded, err := DecodeRelaySourceEvent(submission.OriginalBytes[n])
				if err != nil || decoded.ContentHash != event.ContentHash || decoded.EventUID != event.EventUID {
					return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
				}
			}
		}
		return scopedSourceRetentionPlan{status: status}, nil
	}
	wire, err := EncodeScopedSourceSubmission(submission)
	if err != nil {
		return scopedSourceRetentionPlan{}, err
	}
	bodies, err := submission.sourceOriginalBodies()
	if err != nil {
		return scopedSourceRetentionPlan{}, err
	}
	prior, manifest, err := scopedSourceStatusTx(ctx, tx, op.UID)
	if err == nil {
		if !bytes.Equal(manifest, wire) {
			return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
		}
		return scopedSourceRetentionPlan{status: prior}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return scopedSourceRetentionPlan{}, err
	}
	principalKey, err := ScopedPrincipalKey(op.Principal)
	if err != nil {
		return scopedSourceRetentionPlan{}, err
	}
	if op.IdempotencyKey != "" {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT operation_uid FROM federation_source_operations WHERE source_instance_uid=$1 AND principal_key=$2 AND idempotency_key=$3`, op.SourceInstanceUID, principalKey, op.IdempotencyKey).Scan(&existing)
		if err == nil {
			return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return scopedSourceRetentionPlan{}, err
		}
	}
	for _, member := range op.Members {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT event_uid FROM federation_private_sources WHERE event_uid=$1`, member.EventUID).Scan(&existing)
		if err == nil {
			return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return scopedSourceRetentionPlan{}, err
		}
	}
	dependencies := make([]ScopedSourceDependencyExport, 0, len(op.Membership))
	endpoints := map[string]ScopedEndpoint{}
	for _, proof := range op.Membership {
		if !selected[proof.Endpoint.ProjectUID] {
			return scopedSourceRetentionPlan{}, ErrNotFound
		}
		dependency := ScopedSourceDependencyExport{OperationUID: op.UID, ProjectUID: proof.Endpoint.ProjectUID, IssueUID: proof.Endpoint.IssueUID, ProofBytes: nil, State: "satisfied"}
		switch proof.Kind {
		case "root_membership":
			if err := VerifyScopedMembership(proof, op.Principal, scopedSourcePins(view)); err != nil {
				return scopedSourceRetentionPlan{}, err
			}
		case "component_creation":
			if !uid.Valid(proof.CreatingOperationUID) || len(proof.Signatures) != 0 {
				return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
			}
			dependency.State = "pending"
			creating := submission
			if proof.CreatingOperationUID != op.UID {
				createdStatus, raw, err := scopedSourceStatusTx(ctx, tx, proof.CreatingOperationUID)
				if err != nil {
					return scopedSourceRetentionPlan{}, fmt.Errorf("missing creating operation: %w", err)
				}
				creating, err = DecodeScopedSourceSubmission(raw)
				if err != nil || creating.Operation.Principal != op.Principal || creating.Operation.SourceInstanceUID != op.SourceInstanceUID {
					return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
				}
				if createdStatus.State == "accepted" {
					dependency.State = "satisfied"
				} else if createdStatus.State == "rejected" || createdStatus.State == "canceled" {
					dependency.State = createdStatus.State
					reason := "creating_operation_" + createdStatus.State
					dependency.TerminalReason = &reason
				}
			}
			if !scopedSourceCreation(creating, proof) {
				return scopedSourceRetentionPlan{}, errors.New("membership lacks actual primary creation")
			}
			dependency.EstablishingOperationUID = &proof.CreatingOperationUID
		default:
			return scopedSourceRetentionPlan{}, errors.New("unsupported source membership")
		}
		dependency.ProofBytes, err = json.Marshal(proof)
		if err != nil {
			return scopedSourceRetentionPlan{}, err
		}
		if err := ValidateScopedImportRecord(&dependency); err != nil {
			return scopedSourceRetentionPlan{}, err
		}
		dependencies = append(dependencies, dependency)
		endpoints[proof.Endpoint.IssueUID] = proof.Endpoint
	}
	// Identity correspondence is explicit DATA from the owning native caller.
	// Compatibility callers with no complete relation get the SAME strict
	// factory over unavailable identities; an actual alias lookup then refuses.
	if len(referenceAliases) > 1 {
		return scopedSourceRetentionPlan{}, ErrScopedSourceConflict
	}
	short := NewScopedReferenceAliasResolver(nil, endpoints)
	if len(referenceAliases) == 1 && referenceAliases[0] != nil {
		short = referenceAliases[0]
	}
	for _, event := range submission.Originals {
		_, err := ValidateScopedReferences(event, func(issue string) (ScopedEndpoint, error) {
			endpoint, ok := endpoints[issue]
			if !ok {
				return ScopedEndpoint{}, ErrNotFound
			}
			return endpoint, nil
		}, short)
		if err != nil {
			return scopedSourceRetentionPlan{}, err
		}
	}
	// Opaque expectation bytes are already shape-checked and committed by the
	// manifest. Only root adjudication looks up issuer rows and freshness. Even
	// a root receiving staged work must retain unknown/stale tokens for outcome
	// recovery rather than strand a prior hop's complete-custody ACK.
	principalRaw, err := json.Marshal(op.Principal)
	if err != nil {
		return scopedSourceRetentionPlan{}, err
	}
	admission, err := json.Marshal(ScopedAdmissionEvidence{Version: 1, OperationUID: op.UID, OperationDigest: op.Digest, Principal: op.Principal})
	if err != nil {
		return scopedSourceRetentionPlan{}, err
	}
	row := ScopedSourceOperationExport{OperationUID: op.UID, Digest: op.Digest, SourceInstanceUID: op.SourceInstanceUID, CommandKind: op.CommandKind, ManifestBytes: wire, PrincipalBytes: principalRaw, PrincipalKey: principalKey, State: "staged", OverlayState: "pending", AdmissionBytes: admission, RequestFingerprint: ScopedOfferedDigest(wire)}
	if op.IdempotencyKey != "" {
		row.IdempotencyKey = &op.IdempotencyKey
	}
	plan := scopedSourceRetentionPlan{status: status, born: true, operation: row, dependencies: dependencies}
	if err := ValidateScopedImportRecord(&plan.operation); err != nil {
		return scopedSourceRetentionPlan{}, err
	}
	for n, member := range op.Members {
		ordinal := int64(n)
		original := ScopedPrivateSourceExport{EventUID: member.EventUID, ContentHash: member.ContentHash, ProjectUID: member.ProjectUID, OperationUID: &op.UID, Ordinal: &ordinal, EventBytes: bodies[n]}
		if err := ValidateScopedImportRecord(&original); err != nil {
			return scopedSourceRetentionPlan{}, err
		}
		plan.originals = append(plan.originals, original)
	}
	return plan, nil
}

func retainScopedSourceDataTx(ctx context.Context, tx *sql.Tx, view ScopedViewDescriptor, submission SourceSubmission, referenceAliases ...func(string, string) (ScopedEndpoint, error)) (SourceStatus, bool, error) {
	plan, err := prepareScopedSourceRetentionTx(ctx, tx, view, submission, referenceAliases...)
	if err != nil {
		return SourceStatus{}, false, err
	}
	if !plan.born {
		return plan.status, false, nil
	}
	row := plan.operation
	if err := ReplayScopedRecord(ctx, tx, &row); err != nil {
		return SourceStatus{}, false, err
	}
	for n := range plan.originals {
		if err := ReplayScopedRecord(ctx, tx, &plan.originals[n]); err != nil {
			return SourceStatus{}, false, err
		}
	}
	for n := range plan.dependencies {
		if err := ReplayScopedRecord(ctx, tx, &plan.dependencies[n]); err != nil {
			return SourceStatus{}, false, err
		}
	}
	return plan.status, true, nil
}

func scopedSourceCustodyRow(ctx context.Context, tx *sql.Tx, view ScopedViewDescriptor, op SourceOperation, principal CustodyPrincipal, direction string) (ScopedSourceCustodyExport, error) {
	if err := principal.Validate(); err != nil || principal.AuthorityUID != op.Principal.AuthorityUID || principal.RouteDigest != op.Principal.RouteDigest {
		return ScopedSourceCustodyExport{}, ErrNotFound
	}
	raw, err := scopedSourceCustodyEvidenceTx(ctx, tx, view, principal)
	if err != nil {
		return ScopedSourceCustodyExport{}, err
	}
	row := ScopedSourceCustodyExport{OperationUID: op.UID, BindingUID: principal.BindingUID, Epoch: principal.Epoch, Direction: direction, PrincipalBytes: raw, ManifestDigest: op.Digest, OwnsComplete: true, InvalidationState: "none"}
	return row, ValidateScopedImportRecord(&row)
}

func scopedRetainCustodyTx(ctx context.Context, tx *sql.Tx, view ScopedViewDescriptor, op SourceOperation, principal CustodyPrincipal, direction string) error {
	row, err := scopedSourceCustodyRow(ctx, tx, view, op, principal, direction)
	if err != nil {
		return err
	}
	var old []byte
	var digest string
	err = tx.QueryRowContext(ctx, `SELECT principal_bytes,manifest_digest FROM federation_source_custody WHERE operation_uid=$1 AND binding_uid=$2 AND epoch=$3 AND direction=$4`, op.UID, principal.BindingUID, principal.Epoch, direction).Scan(&old, &digest)
	if err == nil {
		retained, evidence, err := DecodeScopedSourceCustodyPrincipal(old)
		if digest != op.Digest || err != nil || retained != principal {
			return ErrScopedSourceConflict
		}
		if evidence != nil {
			_, actual, err := DecodeScopedSourceCustodyPrincipal(row.PrincipalBytes)
			if err != nil || actual == nil || *evidence != *actual {
				return ErrScopedSourceConflict
			}
		}
		// Legacy exact retries preserve their immutable plain bytes. This
		// explicit native revalidation grants no background view inference.
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return ReplayScopedRecord(ctx, tx, &row)
}

func ScopedSourceBudgetTx(ctx context.Context, tx *sql.Tx) error {
	usage, err := ReadScopedSourceBudgetUsageTx(ctx, tx)
	if err != nil {
		return err
	}
	return usage.validate()
}
