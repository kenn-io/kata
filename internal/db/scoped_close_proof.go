package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/uid"
)

// ScopedCloseClockProof occupies the existing private status_clock_bytes column.
// StatusClock remains the latest native writer. The optional boundary retains
// original intent/invalidation provenance and never grants write authority.
type ScopedCloseClockProof struct {
	Version            int        `json:"version"`
	StatusClock        FoldClock  `json:"status_clock"`
	CloseBoundaryClock *FoldClock `json:"close_boundary_clock"`
}

func (p ScopedCloseClockProof) validate() error {
	if p.Version != 1 || !validCaptureClock(p.StatusClock) {
		return errors.New("invalid close clock proof version or writer")
	}
	if p.CloseBoundaryClock != nil && (*p.CloseBoundaryClock == (FoldClock{}) || !validCaptureClock(*p.CloseBoundaryClock) || compareClock(*p.CloseBoundaryClock, p.StatusClock) > 0) {
		return errors.New("invalid close intent/invalidation boundary")
	}
	return nil
}

// EncodeScopedCloseClockProof encodes derived private clock provenance, never an
// original source envelope. Unknown provenance is explicitly null, not inferred.
func EncodeScopedCloseClockProof(p ScopedCloseClockProof) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, err
	}
	return canonical, nil
}

// DecodeScopedCloseClockProof also recognizes the exact legacy native FoldClock
// object. Legacy bytes establish no boundary; typed values need separate proof.
// Callers retain their exact original bytes/hash rather than replacing them.
func DecodeScopedCloseClockProof(raw []byte) (ScopedCloseClockProof, error) {
	if len(raw) > MaxScopedCheckpointBytes {
		return ScopedCloseClockProof{}, errors.New("close clock proof exceeds private byte limit")
	}
	canonical := jsontext.Value(bytes.Clone(raw))
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return ScopedCloseClockProof{}, err
	}
	var p ScopedCloseClockProof
	if err := json.Unmarshal(canonical, &p, json.RejectUnknownMembers(true)); err == nil {
		encoded, err := EncodeScopedCloseClockProof(p)
		if err != nil {
			return ScopedCloseClockProof{}, err
		}
		if !bytes.Equal(encoded, canonical) {
			return ScopedCloseClockProof{}, errors.New("close clock proof missing fields or invalid nulls")
		}
		return p, nil
	}
	var legacy FoldClock
	if err := json.Unmarshal(canonical, &legacy, json.RejectUnknownMembers(true)); err != nil {
		return ScopedCloseClockProof{}, err
	}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		return ScopedCloseClockProof{}, err
	}
	typed := jsontext.Value(encoded)
	if err := typed.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return ScopedCloseClockProof{}, err
	}
	if !bytes.Equal(typed, canonical) || !validCaptureClock(legacy) {
		return ScopedCloseClockProof{}, errors.New("invalid legacy close clock shape")
	}
	return ScopedCloseClockProof{Version: 1, StatusClock: legacy}, nil
}

// ValidateScopedCanonicalCloseState validates private proof-to-subject/status
// consistency. Native transactions separately recheck live project membership,
// authorization and source admission; this helper grants none of those.
func ValidateScopedCanonicalCloseState(p CanonicalCloseState, subjectUID, status string) error {
	if !uid.Valid(subjectUID) || p.IssueUID != subjectUID || (status != "open" && status != "closed") || (p.StatusIntentUID != "" && !uid.Valid(p.StatusIntentUID)) {
		return errors.New("invalid canonical close subject, status or intent")
	}
	if err := (ScopedCloseClockProof{Version: 1, StatusClock: p.StatusClock, CloseBoundaryClock: p.CloseBoundaryClock}).validate(); err != nil {
		return err
	}
	if p.StatusIntentUID != "" && p.StatusClock == (FoldClock{}) {
		return errors.New("status intent without writer")
	}
	if status == "closed" && p.StatusIntentUID != "" && p.CloseBoundaryClock != nil && p.CloseBoundaryClock.EventUID != p.StatusIntentUID {
		return errors.New("closed intent/boundary disagreement")
	}
	if p.Value != nil {
		if err := p.Value.Validate(); err != nil {
			return err
		}
		if p.Value.PeerIssueUID == subjectUID || status != "closed" || p.StatusIntentUID == "" || p.CloseBoundaryClock == nil || p.CloseBoundaryClock.EventUID != p.StatusIntentUID {
			return errors.New("close value lacks surviving original intent proof")
		}
	}
	return nil
}

func cloneCloseBoundary(v *FoldClock) *FoldClock {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}
func cloneCloseValue(v *ScopedCloseValue) *ScopedCloseValue {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}

// decodeNativeCloseMachine inspects only the two reserved typed machine fields;
// ordinary source fields remain native content. Duplicate members are rejected
// by json/v2, and each present machine value has a closed required-field shape.
func decodeNativeCloseMachine(kind string, raw []byte) (*ScopedCloseValue, *CanonicalCloseState, error) {
	// Reserved carriers are top-level object fields. Other JSON values keep
	// their native validation path (including legacy metadata no-op events).
	object := bytes.TrimSpace(raw)
	if len(object) == 0 || object[0] != '{' {
		return nil, nil, nil
	}
	// Legacy families may use unrelated extension fields with these names.
	// Detect reserved carriers before imposing close-specific subject types.
	var carriers struct {
		Value jsontext.Value `json:"close_value"`
		State jsontext.Value `json:"close_state"`
	}
	if err := json.Unmarshal(raw, &carriers); err != nil {
		return nil, nil, err
	}
	if len(carriers.Value) == 0 && len(carriers.State) == 0 {
		return nil, nil, nil
	}
	var fields struct {
		UID    string         `json:"uid"`
		Status string         `json:"status"`
		Reason string         `json:"reason"`
		Value  jsontext.Value `json:"close_value"`
		State  jsontext.Value `json:"close_state"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	if len(fields.Value) > 0 {
		if kind != "issue.closed" || bytes.Equal(fields.Value, []byte("null")) {
			return nil, nil, errors.New("close_value is only a typed close original")
		}
		var value ScopedCloseValue
		if err := json.Unmarshal(fields.Value, &value, json.RejectUnknownMembers(true)); err != nil {
			return nil, nil, err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, nil, err
		}
		typed := jsontext.Value(encoded)
		supplied := jsontext.Value(bytes.Clone(fields.Value))
		if err := typed.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
			return nil, nil, err
		}
		if err := supplied.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(typed, supplied) {
			return nil, nil, errors.New("close_value missing required fields or invalid nulls")
		}
		if err := value.Validate(); err != nil {
			return nil, nil, err
		}
		if fields.Reason != value.Kind || (fields.UID != "" && value.PeerIssueUID == fields.UID) {
			return nil, nil, errors.New("close value reason or subject mismatch")
		}
		if len(fields.State) > 0 {
			return nil, nil, errors.New("mixed native close carriers")
		}
		return &value, nil, nil
	}
	if (kind != "issue.created" && kind != "issue.snapshot") || bytes.Equal(fields.State, []byte("null")) {
		return nil, nil, errors.New("close_state is only a typed current snapshot")
	}
	var state CanonicalCloseState
	if err := json.Unmarshal(fields.State, &state, json.RejectUnknownMembers(true)); err != nil {
		return nil, nil, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, nil, err
	}
	typed := jsontext.Value(encoded)
	supplied := jsontext.Value(bytes.Clone(fields.State))
	if err := typed.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, nil, err
	}
	if err := supplied.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(typed, supplied) {
		return nil, nil, errors.New("close_state missing required fields or invalid nulls")
	}
	if err := ValidateScopedCanonicalCloseState(state, fields.UID, fields.Status); err != nil {
		return nil, nil, err
	}
	return nil, &state, nil
}

func validateNativeCloseClock(proof *CanonicalCloseState, clock FoldClock) error {
	if proof != nil && compareClock(proof.StatusClock, clock) > 0 {
		return errors.New("snapshot proof exceeds native envelope writer")
	}
	return nil
}

func sameCloseBoundary(a, b *FoldClock) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
func sameCloseValue(a, b *ScopedCloseValue) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// AdvanceScopedCanonicalClose advances current SQL status provenance using the
// existing native fold and one newly committed local event. It never scans
// history or treats a decoded capture as an incremental fold accumulator.
// Remote materializers persist their existing full native fold result instead.
func AdvanceScopedCanonicalClose(current FoldIssue, event FoldEvent) (FoldIssue, error) {
	switch event.Type {
	case "issue.created", "issue.snapshot", "issue.closed", "issue.reopened":
	case "issue.updated":
		if status, ok := stringValue(PayloadMap(event.Payload)["status"]); !ok || status == "" {
			return current, nil
		}
	default:
		return current, nil
	}
	if event.IssueUID != current.UID || current.UID == "" {
		return current, errors.New("current close event subject mismatch")
	}
	if compareClock(clockOf(event), current.StatusClock) <= 0 {
		return current, nil
	}
	projection := FoldEvents(nil)
	projection.Issues[current.UID] = current
	projection.apply(event)
	if len(projection.Warnings) != 0 {
		return current, errors.New(projection.Warnings[0])
	}
	return projection.Issues[current.UID], nil
}

// NativeClosePeerReference reads only the declared peer in the reserved native
// typed carriers. It never resolves prose, infers ownership or rewrites source
// bytes. Native hop validators separately check declared/current membership.
func NativeClosePeerReference(eventType string, raw []byte) (*ScopedEndpoint, error) {
	value, state, err := decodeNativeCloseMachine(eventType, raw)
	if err != nil {
		return nil, err
	}
	if state != nil {
		value = state.Value
	}
	if value == nil {
		return nil, nil
	}
	return &ScopedEndpoint{ProjectUID: value.PeerProjectUID, IssueUID: value.PeerIssueUID}, nil
}
