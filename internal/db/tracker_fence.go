package db

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"strings"

	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

// TrackerMetadataPrefix marks owner-local execution state. Project exports and
// imports must not carry it as portable project authority.
const TrackerMetadataPrefix = "tracker_fence_v1."

var (
	// ErrTrackerFenceChanged means the retained fence no longer matches.
	ErrTrackerFenceChanged = errors.New("tracker execution fence changed")
	// ErrTrackerOutcomeUnknown blocks dispatch when provider history is uncertain.
	ErrTrackerOutcomeUnknown = errors.New("tracker provider outcome is unknown; automatic dispatch is blocked")
	// ErrTrackerFenceInvalid means execution metadata violates its contract.
	ErrTrackerFenceInvalid = errors.New("invalid tracker execution fence")
)

// TrackerWritePhase identifies a durable provider-write state.
type TrackerWritePhase string

// Execution phases retain whether a write may dispatch or has settled.
const (
	// TrackerWritePrepared starts the write phase sequence.
	TrackerWritePrepared         TrackerWritePhase = "prepared"
	TrackerWriteDispatching      TrackerWritePhase = "dispatching"
	TrackerWriteConfirmed        TrackerWritePhase = "confirmed"
	TrackerWriteAcknowledged     TrackerWritePhase = "acknowledged"
	TrackerWriteUnknown          TrackerWritePhase = "unknown"
	TrackerWriteConvergedUnknown TrackerWritePhase = "converged_unknown"
	TrackerWriteNotSent          TrackerWritePhase = "not_sent"
)

// TrackerExecutionFence stores one serialized write and its acknowledged head
// in the existing meta table. It is not a general job queue or a provider token.
// Unknown writes never expire into permission to retry, even after claim expiry.
type TrackerExecutionFence struct {
	Remote               *TrackerRemoteAttempt `json:"remote,omitempty"`
	Version              int                   `json:"version"`
	Role                 string                `json:"role"`
	RootInstanceUID      string                `json:"root_instance_uid"`
	ProjectUID           string                `json:"project_uid"`
	Connection           IssueSyncConnection   `json:"connection"`
	Sequence             uint64                `json:"sequence"`
	WriteUID             string                `json:"write_uid"`
	PreviousHead         string                `json:"previous_head,omitempty"`
	PendingEventUID      string                `json:"pending_event_uid"`
	SourceKey            string                `json:"source_key"`
	Locator              string                `json:"locator"`
	Target               string                `json:"target"`
	ProcessEpoch         string                `json:"process_epoch"`
	Phase                TrackerWritePhase     `json:"phase"`
	ProviderAcknowledged bool                  `json:"provider_acknowledged"`
	ActivationRequired   bool                  `json:"activation_required"`
	Result               jsontext.Value        `json:"result,omitempty"`
}

// CanDispatch is only a local prerequisite. Live service, project, connection,
// pending-event and post-pacing root admission checks are additionally required.
func (f TrackerExecutionFence) CanDispatch(processEpoch string) bool {
	return f.Validate() == nil && f.Phase == TrackerWritePrepared && !f.ActivationRequired && f.ProcessEpoch == processEpoch
}

// Validate checks the execution fence and its phase evidence.
func (f TrackerExecutionFence) Validate() error {
	if f.Version != 1 || (f.Role != "coordinator" && f.Role != "executor") ||
		!uid.Valid(f.RootInstanceUID) || !uid.Valid(f.ProjectUID) || !uid.Valid(f.PendingEventUID) || !uid.Valid(f.ProcessEpoch) ||
		f.Connection.Validate() != nil || f.Sequence == 0 || f.Sequence > math.MaxInt64 || !trackerDigest(f.WriteUID) ||
		(f.Sequence == 1 && f.PreviousHead != "") || (f.Sequence > 1 && !trackerDigest(f.PreviousHead)) ||
		!trackerIdentityText(f.SourceKey) || !trackerIdentityText(f.Locator) || (f.Target != "open" && f.Target != "closed") {
		return ErrTrackerFenceInvalid
	}
	if f.Remote != nil {
		if f.Remote.Validate() != nil || (f.Role == "coordinator" && (f.Remote.RootSequence != f.Sequence || f.Remote.RootPreviousHead != f.PreviousHead || f.Remote.RootEpoch != f.ProcessEpoch)) || (f.Role == "executor" && (f.Remote.OwnerSequence != f.Sequence || f.Remote.OwnerPreviousHead != f.PreviousHead)) {
			return ErrTrackerFenceInvalid
		}
	}
	expected, err := TrackerStatusWriteUID(f.RootInstanceUID, f.ProjectUID, f.SourceKey, f.Locator, f.PendingEventUID, f.Target)
	if err != nil || f.WriteUID != expected {
		return ErrTrackerFenceInvalid
	}
	if len(f.Result) > 16<<10 || (len(f.Result) > 0 && (!f.Result.IsValid() || len(bytes.TrimSpace(f.Result)) == 0 || bytes.TrimSpace(f.Result)[0] != '{')) {
		return ErrTrackerFenceInvalid
	}
	switch f.Phase {
	case TrackerWritePrepared, TrackerWriteDispatching, TrackerWriteUnknown:
		if f.ProviderAcknowledged || len(f.Result) != 0 {
			return ErrTrackerFenceInvalid
		}
	case TrackerWriteConvergedUnknown:
		if f.ProviderAcknowledged || len(f.Result) == 0 {
			return ErrTrackerFenceInvalid
		}
	case TrackerWriteNotSent:
		if f.ProviderAcknowledged || len(f.Result) == 0 {
			return ErrTrackerFenceInvalid
		}
	case TrackerWriteConfirmed, TrackerWriteAcknowledged:
		if !f.ProviderAcknowledged || len(f.Result) == 0 {
			return ErrTrackerFenceInvalid
		}
	default:
		return ErrTrackerFenceInvalid
	}
	return nil
}

// TrackerStatusWriteUID is independent of caller/run/connection generation, so
// replacing one of those cannot manufacture a fresh identity for an old intent.
func TrackerStatusWriteUID(root, project, source, locator, event, target string) (string, error) {
	if !uid.Valid(root) || !uid.Valid(project) || !uid.Valid(event) || !trackerIdentityText(source) || !trackerIdentityText(locator) || (target != "open" && target != "closed") {
		return "", ErrTrackerFenceInvalid
	}
	raw, err := json.Marshal([7]string{"kata:tracker-write:v1", root, project, source, locator, event, target})
	if err != nil {
		return "", ErrTrackerFenceInvalid
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// TrackerFenceHead commits to the sequence and its predecessor. A restored root
// behind the owner's head cannot skip forward or replay its older write.
func TrackerFenceHead(f TrackerExecutionFence) string {
	parts := []any{"kata:tracker-head:v1", f.RootInstanceUID, f.ProjectUID, f.Sequence, f.PreviousHead, f.WriteUID, f.Connection}
	if f.Remote != nil {
		parts = append(parts, *f.Remote)
	}
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ValidateTrackerFenceTransition checks whether one durable state may advance.
func ValidateTrackerFenceTransition(previous, next TrackerExecutionFence) error {
	if previous.Validate() != nil || next.Validate() != nil {
		return ErrTrackerFenceInvalid
	}
	if previous.Role != next.Role || previous.ProjectUID != next.ProjectUID || previous.RootInstanceUID != next.RootInstanceUID {
		return ErrTrackerFenceChanged
	}
	if next.Sequence != previous.Sequence {
		if !trackerFenceSettled(previous) {
			return ErrTrackerOutcomeUnknown
		}
		if previous.ActivationRequired || next.ActivationRequired || next.Sequence != previous.Sequence+1 || next.PreviousHead != TrackerFenceHead(previous) || next.Phase != TrackerWritePrepared {
			return ErrTrackerFenceChanged
		}
		if next.WriteUID == previous.WriteUID && (previous.Phase != TrackerWriteNotSent || previous.Connection != next.Connection || previous.ProcessEpoch != next.ProcessEpoch || previous.Remote != nil || next.Remote != nil) {
			return ErrTrackerOutcomeUnknown
		}
		return nil
	}
	if (previous.Remote == nil) != (next.Remote == nil) || (previous.Remote != nil && *previous.Remote != *next.Remote) {
		return ErrTrackerFenceChanged
	}
	if previous.WriteUID != next.WriteUID || previous.PreviousHead != next.PreviousHead || previous.Connection != next.Connection || previous.PendingEventUID != next.PendingEventUID ||
		previous.SourceKey != next.SourceKey || previous.Locator != next.Locator || previous.Target != next.Target || previous.ProcessEpoch != next.ProcessEpoch || (previous.ActivationRequired && !next.ActivationRequired) {
		return ErrTrackerFenceChanged
	}
	if previous.Phase == next.Phase {
		left, _ := json.Marshal(previous)
		right, _ := json.Marshal(next)
		if bytes.Equal(left, right) {
			return nil
		}
		return ErrTrackerFenceChanged
	}
	allowed := false
	if next.Phase == TrackerWriteDispatching && (previous.ActivationRequired || next.ActivationRequired) {
		return ErrTrackerOutcomeUnknown
	}
	switch previous.Phase {
	case TrackerWritePrepared:
		allowed = next.Phase == TrackerWriteDispatching || next.Phase == TrackerWriteUnknown || next.Phase == TrackerWriteNotSent
	case TrackerWriteDispatching:
		allowed = next.Phase == TrackerWriteConfirmed || next.Phase == TrackerWriteUnknown
	case TrackerWriteUnknown:
		allowed = next.Phase == TrackerWriteConfirmed || next.Phase == TrackerWriteConvergedUnknown
	case TrackerWriteConvergedUnknown:
		allowed = next.Phase == TrackerWriteConfirmed
	case TrackerWriteConfirmed:
		allowed = next.Phase == TrackerWriteAcknowledged && bytes.Equal(previous.Result, next.Result)
	}
	if !allowed {
		return ErrTrackerOutcomeUnknown
	}
	return nil
}

func trackerFenceSettled(f TrackerExecutionFence) bool {
	return f.Phase == TrackerWriteAcknowledged || f.Phase == TrackerWriteNotSent
}

func trackerDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func trackerIdentityText(s string) bool {
	return tracker.ValidIdentityText(s, 2048)
}
