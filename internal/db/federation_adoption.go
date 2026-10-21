package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"

	katauid "go.kenn.io/kata/internal/uid"
)

// FederationAdoptionOperation is the secret-free durable source pause. Artifact,
// digest and catalog fields are opaque to the native store.
type FederationAdoptionOperation struct {
	OperationUID, SourceInstanceUID, ProjectUID, TargetInstanceUID string
	TargetOrigin, CatalogName, Account, LocalAccount, ProjectName  string
	EnvelopePath, Digest, Phase                                    string
	ProjectID, HubProjectID                                        int64
	BindingUID                                                     string
	ResetEpoch                                                     int64
}

// FederationAdoptionStore is the optional durable source-operation API.
type FederationAdoptionStore interface {
	BeginFederationAdoption(context.Context, FederationAdoptionOperation) (FederationAdoptionOperation, error)
	FederationAdoption(context.Context, int64) (FederationAdoptionOperation, error)
	AdvanceFederationAdoption(context.Context, FederationAdoptionOperation, FederationAdoptionOperation) error
	CancelUnusedFederationAdoption(context.Context, FederationAdoptionOperation) error
	FinalizeFederationAdoption(context.Context, FederationAdoptionOperation, FederationBinding) (bool, error)
}

// ErrFederationAdoptionPaused rejects ordinary writes until cutover completes.
var ErrFederationAdoptionPaused = errors.New("project writes paused for relay adoption")

// ErrFederationAdoptionConflict rejects stale or unsafe operation transitions.
var ErrFederationAdoptionConflict = errors.New("relay adoption operation changed or transition refused")

const federationAdoptionPrefix = "federation.adoption.v1/"

type adoptionAttachmentKey struct{}

// WithFederationAdoptionAttachment permits only the exact replica control
// transition. Ordinary content gates deliberately ignore this context.
func WithFederationAdoptionAttachment(ctx context.Context, operation FederationAdoptionOperation) context.Context {
	return context.WithValue(ctx, adoptionAttachmentKey{}, operation)
}

// FederationAdoptionCredentialValidator must re-read the exact activated managed
// credential under transition/rebind coordination and retain that coordination
// until the native transaction commits. A generic authorization fence is not
// this proof. The callback receives the transaction's current operation/binding.
type FederationAdoptionCredentialValidator func(context.Context, Transaction, FederationAdoptionOperation, FederationBinding) error
type adoptionFinalizationProof struct {
	operation FederationAdoptionOperation
	binding   FederationBinding
	validate  FederationAdoptionCredentialValidator
}
type adoptionFinalizationKey struct{}

// WithFederationAdoptionFinalization binds a live credential validator to the
// exact operation and binding. The caller retains rebind coordination to commit.
func WithFederationAdoptionFinalization(ctx context.Context, operation FederationAdoptionOperation, binding FederationBinding, validate FederationAdoptionCredentialValidator) context.Context {
	return context.WithValue(ctx, adoptionFinalizationKey{}, adoptionFinalizationProof{operation, binding, validate})
}

// FederationAdoptionSQL reuses the native transaction; stores acquire project
// coordination before invoking it. Both engines support numbered parameters.
type FederationAdoptionSQL struct {
	Tx          Transaction
	InstanceUID string
}

// Read loads a source operation without changing or locking portable state.
func (s FederationAdoptionSQL) Read(ctx context.Context, projectID int64) (FederationAdoptionOperation, error) {
	var raw string
	err := s.Tx.QueryRowContext(ctx, `SELECT m.value FROM meta m JOIN projects p ON m.key=$1 || p.uid WHERE p.id=$2`, federationAdoptionPrefix, projectID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return FederationAdoptionOperation{}, ErrNotFound
	}
	if err != nil {
		return FederationAdoptionOperation{}, err
	}
	var op FederationAdoptionOperation
	if err := json.Unmarshal([]byte(raw), &op); err != nil {
		return op, fmt.Errorf("decode relay adoption: %w", err)
	}
	if op.ProjectID != projectID || op.SourceInstanceUID != s.InstanceUID {
		return op, ErrFederationAdoptionConflict
	}
	return op, nil
}

// Begin rechecks native eligibility and installs a source pause under the
// caller's exclusive project coordination. It never validates envelope content.
func (s FederationAdoptionSQL) Begin(ctx context.Context, op FederationAdoptionOperation) (FederationAdoptionOperation, error) {
	if op.Phase == "" {
		op.Phase = "preparing"
	}
	if !katauid.Valid(op.OperationUID) || !katauid.Valid(op.ProjectUID) || !katauid.Valid(op.TargetInstanceUID) || op.SourceInstanceUID != s.InstanceUID || op.TargetInstanceUID == s.InstanceUID || op.ProjectID <= 0 || op.Account == "" || (op.LocalAccount != "" && ValidateTokenActor(op.LocalAccount) != nil) || op.TargetOrigin == "" || op.CatalogName == "" || op.ProjectUID == SystemProjectUID {
		return op, ErrFederationAdoptionConflict
	}
	current, err := s.Read(ctx, op.ProjectID)
	if err == nil {
		if current == op {
			return current, nil
		}
		return op, ErrFederationAdoptionConflict
	}
	if !errors.Is(err, ErrNotFound) {
		return op, err
	}
	if op.Phase != "preparing" || op.EnvelopePath != "" || op.Digest != "" || op.HubProjectID != 0 || op.BindingUID != "" || op.ResetEpoch != 0 {
		return op, ErrFederationAdoptionConflict
	}
	var uid, name string
	var archived sql.NullString
	if err := s.Tx.QueryRowContext(ctx, `SELECT uid,name,CAST(deleted_at AS TEXT) FROM projects WHERE id=$1`, op.ProjectID).Scan(&uid, &name, &archived); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return op, ErrNotFound
		}
		return op, err
	}
	if uid != op.ProjectUID || name != op.ProjectName || archived.Valid || name == SystemProjectName {
		return op, ErrFederationAdoptionConflict
	}
	// Recheck mutable authority/lifecycle after earlier native writers drain.
	var blocked bool
	err = s.Tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM federation_bindings WHERE project_id=$1) OR
 EXISTS(SELECT 1 FROM federation_root_keys WHERE project_uid=$2) OR
 EXISTS(SELECT 1 FROM federation_event_provenance WHERE project_uid=$2) OR
 EXISTS(SELECT 1 FROM federation_entity_provenance WHERE project_uid=$2) OR
 EXISTS(SELECT 1 FROM federation_enrollments WHERE project_id=$1 AND revoked_at IS NULL) OR
 EXISTS(SELECT 1 FROM issue_sync_bindings b LEFT JOIN issue_sync_status s ON s.binding_id=b.id WHERE b.project_id=$1 AND (b.enabled=1 OR s.sync_started_at IS NOT NULL)) OR
 EXISTS(SELECT 1 FROM external_root_bindings WHERE project_id=$1 AND (active=1 OR claim_token<>'')) OR
 EXISTS(SELECT 1 FROM issue_claims WHERE project_id=$1 AND released_at IS NULL) OR
 EXISTS(SELECT 1 FROM pending_claim_requests WHERE project_id=$1 AND rejected_at IS NULL AND resolved_at IS NULL) OR
 EXISTS(SELECT 1 FROM federation_quarantine WHERE project_id=$1 AND skipped_at IS NULL) OR
 EXISTS(SELECT 1 FROM recurrences WHERE project_id=$1) OR
 EXISTS(SELECT 1 FROM cron_jobs WHERE project_id=$1) OR
 EXISTS(SELECT 1 FROM cron_workflows WHERE project_id=$1) OR
 EXISTS(SELECT 1 FROM cron_runs WHERE project_id=$1)`, op.ProjectID, op.ProjectUID).Scan(&blocked)
	if err != nil {
		return op, err
	}
	if blocked {
		return op, ErrFederationAdoptionConflict
	}
	return op, s.write(ctx, op)
}

func (s FederationAdoptionSQL) write(ctx context.Context, op FederationAdoptionOperation) error {
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	_, err = s.Tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, federationAdoptionPrefix+op.ProjectUID, string(raw))
	return err
}

// Advance compares the complete expected operation before a forward transition.
func (s FederationAdoptionSQL) Advance(ctx context.Context, expected, next FederationAdoptionOperation) error {
	current, err := s.Read(ctx, expected.ProjectID)
	if err != nil {
		return err
	}
	if current != expected {
		return ErrFederationAdoptionConflict
	}
	immutable := next
	immutable.Phase = expected.Phase
	immutable.EnvelopePath = expected.EnvelopePath
	immutable.Digest = expected.Digest
	immutable.HubProjectID = expected.HubProjectID
	immutable.BindingUID = expected.BindingUID
	immutable.ResetEpoch = expected.ResetEpoch
	if immutable != expected {
		return ErrFederationAdoptionConflict
	}
	if expected == next {
		return nil
	}
	allowed := false
	switch expected.Phase {
	case "preparing":
		allowed = next.Phase == "frozen" && next.EnvelopePath != "" && next.Digest != "" && next.HubProjectID == 0 && next.BindingUID == "" && next.ResetEpoch == 0
	case "frozen":
		allowed = next.Phase == "transmitting" || next.Phase == "rejected"
	case "transmitting":
		allowed = next.Phase == "published" && next.HubProjectID > 0 && next.BindingUID == "" && next.ResetEpoch == 0 || next.Phase == "rejected"
	case "attached":
		allowed = next.Phase == "attached" && next.BindingUID == expected.BindingUID && next.HubProjectID == expected.HubProjectID && next.ResetEpoch > expected.ResetEpoch
	case "published":
		allowed = (next.Phase == "published" || next.Phase == "attached") && next.HubProjectID == expected.HubProjectID && next.BindingUID != "" && next.ResetEpoch > 0 && (expected.BindingUID == "" || expected.BindingUID == next.BindingUID)
	}
	if expected.Phase != "preparing" && (next.EnvelopePath != expected.EnvelopePath || next.Digest != expected.Digest) {
		allowed = false
	}
	if expected.Phase == "frozen" && (next.HubProjectID != 0 || next.BindingUID != "" || next.ResetEpoch != 0) {
		allowed = false
	}
	if next.Phase == "rejected" && (expected.HubProjectID != 0 || next.HubProjectID != 0 || next.BindingUID != "" || next.ResetEpoch != 0) {
		allowed = false
	}
	if !allowed {
		return ErrFederationAdoptionConflict
	}
	return s.write(ctx, next)
}

// Cancel releases only an exact operation proven unused by its durable phase.
func (s FederationAdoptionSQL) Cancel(ctx context.Context, expected FederationAdoptionOperation) error {
	current, err := s.Read(ctx, expected.ProjectID)
	if err != nil {
		return err
	}
	if current != expected || current.HubProjectID != 0 || (current.Phase != "preparing" && current.Phase != "frozen" && current.Phase != "rejected") {
		return ErrFederationAdoptionConflict
	}
	_, err = s.Tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, federationAdoptionPrefix+current.ProjectUID)
	return err
}

// CheckAttachment permits only the exact published replica control transition.
func (s FederationAdoptionSQL) CheckAttachment(ctx context.Context, projectID int64, binding *FederationBinding) error {
	current, err := s.Read(ctx, projectID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	expected, ok := ctx.Value(adoptionAttachmentKey{}).(FederationAdoptionOperation)
	if !ok || expected != current || (current.Phase != "published" && current.Phase != "attached") {
		return ErrFederationAdoptionPaused
	}
	if binding != nil && !FederationAdoptionBindingMatches(current, *binding) {
		return ErrFederationAdoptionPaused
	}
	return nil
}

// FederationAdoptionBindingMatches checks the resolved destination and hop.
func FederationAdoptionBindingMatches(op FederationAdoptionOperation, b FederationBinding) bool {
	if b.ProjectID != op.ProjectID || b.Role != FederationRoleSpoke || !b.Enabled || !b.PushEnabled || b.HubURL != op.TargetOrigin || b.HubProjectID != op.HubProjectID || b.HubProjectUID != op.ProjectUID || b.Actor != op.Account {
		return false
	}
	return b.RelayConfig == nil || b.RelayConfig.BindingUID == op.BindingUID && b.RelayConfig.ResetEpoch == op.ResetEpoch && b.RelayConfig.UpstreamInstanceUID == op.TargetInstanceUID
}

// ValidateSyncBinding admits dedicated sync at the operation's epoch, or
// a newer epoch proven by the retained authenticated reset for this exact hop.
// It never advances the operation or proves activated local credentials.
func (s FederationAdoptionSQL) ValidateSyncBinding(ctx context.Context, operation FederationAdoptionOperation, binding FederationBinding, reset RelayResetProtocol) error {
	if operation.Phase != "published" && operation.Phase != "attached" || binding.RelayConfig == nil {
		return ErrFederationAdoptionPaused
	}
	if FederationAdoptionBindingMatches(operation, binding) {
		return nil
	}
	config := binding.RelayConfig
	current := operation
	current.ResetEpoch = config.ResetEpoch
	if current.ResetEpoch <= operation.ResetEpoch || !FederationAdoptionBindingMatches(current, binding) || config.Validate(s.InstanceUID) != nil {
		return ErrFederationAdoptionPaused
	}
	raw, found, err := reset.Backend.Meta(ctx, RelayResetMetadataPrefix+operation.ProjectUID)
	if err != nil {
		return err
	}
	if !found {
		return ErrFederationAdoptionPaused
	}
	var checkpoint RelayResetCheckpoint
	if err := json.Unmarshal([]byte(raw), &checkpoint, json.RejectUnknownMembers(true)); err != nil {
		return errors.Join(ErrFederationAdoptionPaused, err)
	}
	pin, err := reset.Backend.RootPin(ctx, operation.ProjectUID, checkpoint.Manifest.KeyID)
	if err != nil {
		return errors.Join(ErrFederationAdoptionPaused, err)
	}
	authority := RelayHopAuthority{BindingUID: operation.BindingUID, ProjectUID: operation.ProjectUID, AuthorityUID: config.AuthorityUID, SenderInstanceUID: operation.TargetInstanceUID, ReceiverInstanceUID: s.InstanceUID, Epoch: config.ResetEpoch}
	if err := ValidateRelayResetTranslation(authority, checkpoint.Manifest, checkpoint.Translation); err != nil {
		return errors.Join(ErrFederationAdoptionPaused, err)
	}
	if err := VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot); err != nil {
		return errors.Join(ErrFederationAdoptionPaused, err)
	}
	return nil
}

// ValidateFinalization rejects generic/no-op authorization fences and stale
// callback snapshots before invoking the required live credential validator.
func (s FederationAdoptionSQL) ValidateFinalization(ctx context.Context, expected FederationAdoptionOperation, binding FederationBinding) error {
	current, err := s.Read(ctx, expected.ProjectID)
	if err != nil {
		return err
	}
	if current != expected || current.Phase != "attached" {
		return ErrFederationAdoptionConflict
	}
	return s.ValidateCredentialProof(ctx, current, binding)
}

// ValidateCredentialProof is also used when an authenticated reset advances the
// current binding epoch. The proof names the desired operation with that epoch;
// the caller must retain credential/rebind coordination through commit.
func (s FederationAdoptionSQL) ValidateCredentialProof(ctx context.Context, operation FederationAdoptionOperation, binding FederationBinding) error {
	proof, ok := ctx.Value(adoptionFinalizationKey{}).(adoptionFinalizationProof)
	if !ok || proof.operation != operation || proof.validate == nil || !reflect.DeepEqual(proof.binding, binding) || binding.RelayConfig == nil || !FederationAdoptionBindingMatches(operation, binding) {
		return ErrFederationAdoptionConflict
	}
	return proof.validate(ctx, s.Tx, operation, binding)
}

// Release deletes this operation's marker after native finalization checks.
func (s FederationAdoptionSQL) Release(ctx context.Context, op FederationAdoptionOperation) error {
	_, err := s.Tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, federationAdoptionPrefix+op.ProjectUID)
	return err
}
