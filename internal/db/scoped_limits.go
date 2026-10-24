package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
)

// Admission limits bound retained work; they are never eviction deadlines.
const (
	ScopedRelayCapability            = "relay_scoped_links_v1"
	ScopedRelayProtocolVersion       = 2
	ScopedProjectionSchema           = 1
	MaxScopedActiveViews             = 128
	MaxScopedSelectedProjects        = 64
	MaxScopedComponentOperations     = 256
	MaxScopedComponentOriginalBytes  = 4 << 20
	MaxScopedCheckpointBytes         = 64 << 20
	MaxScopedEnvelopeBytes           = (MaxScopedCheckpointBytes * 4 / 3) + (64 << 10)
	MaxScopedRecoveryBytes           = 64 << 20
	MaxScopedBindingBytes            = 256 << 20
	MaxScopedDaemonBytes             = 2 << 30
	MaxScopedActiveGenerations       = 1
	MaxScopedPreparedGenerations     = 1
	ScopedPreparationLifetimeSeconds = 7 * 24 * 60 * 60
)

const (
	ScopedViewDomain       = "kata.scoped.view.v1"
	ScopedEntityDomain     = "kata.scoped.entity.v1"
	ScopedReferenceDomain  = "kata.scoped.reference.v1"
	ScopedProvenanceDomain = "kata.scoped.provenance.v1"
	ScopedBatchDomain      = "kata.scoped.batch.v1"
	ScopedCheckpointDomain = "kata.scoped.checkpoint.v1"
	ScopedMembershipDomain = "kata.scoped.membership.v1"
	ScopedResultDomain     = "kata.scoped.result.v1"
	ScopedCoverageDomain   = "kata.scoped.coverage.v1"
	ScopedCutoverDomain    = "kata.scoped.cutover.v1"
)

// ScopedSigningBytes wraps an unsigned record in a closed domain. Callers must
// omit signatures before calling. Integer precision is preserved at int64
// boundaries; object order is canonical and duplicate members are rejected.
func ScopedSigningBytes(domain string, unsigned []byte) ([]byte, error) {
	switch domain {
	case ScopedViewDomain, ScopedEntityDomain, ScopedReferenceDomain, ScopedProvenanceDomain, ScopedBatchDomain, ScopedCheckpointDomain, ScopedMembershipDomain, ScopedResultDomain, ScopedCoverageDomain, ScopedCutoverDomain:
	default:
		return nil, errors.New("unknown scoped signature domain")
	}
	record := jsontext.Value(bytes.Clone(unsigned))
	if err := record.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Domain string         `json:"domain"`
		Record jsontext.Value `json:"record"`
	}{domain, record})
}

// DecodeScopedRecord refuses unknown/duplicate fields and calls the record's
// shape validator when present. Expected identity, authority and signed-record
// semantics are checked by the owning consumer, not inferred from JSON decoding.
// Each input is independent; publish a fresh value only after it validates.
func DecodeScopedRecord(raw []byte, out any) error {
	destination := reflect.ValueOf(out)
	if !destination.IsValid() || destination.Kind() != reflect.Pointer || destination.IsNil() {
		return errors.New("scoped record destination must be a nonnil pointer")
	}
	fresh := reflect.New(destination.Elem().Type())
	if err := json.Unmarshal(raw, fresh.Interface(), json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	if validator, ok := fresh.Interface().(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return err
		}
	}
	destination.Elem().Set(fresh.Elem())
	return nil
}

// ValidateScopedSignedBytes rejects alternate encodings of a signed transcript.
func ValidateScopedSignedBytes(raw []byte) error {
	canonical := jsontext.Value(bytes.Clone(raw))
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return err
	}
	if !bytes.Equal(raw, canonical) {
		return errors.New("noncanonical scoped signed bytes")
	}
	return nil
}

func ValidateScopedCheckpointSize(n int64) error {
	if n < 0 || n > MaxScopedCheckpointBytes {
		return fmt.Errorf("scoped checkpoint exceeds %d bytes", MaxScopedCheckpointBytes)
	}
	return nil
}

func scopedDigest(value string) bool {
	return validRelayDigest(value)
}
