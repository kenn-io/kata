package db

import (
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/uid"
)

// Persisted-row limits for retained embedding artifacts. The schema carries no
// CHECK constraints for them; every native write and import applies these.
const (
	MaxRetainedArtifactManifestBytes = 2 << 20
	MaxRetainedArtifactBytes         = 32 << 20
)

// ValidateRetainedArtifactBytes bounds the serialized manifest and artifact a
// store retains, so an oversized record is refused before it is written.
func ValidateRetainedArtifactBytes(manifestRaw, artifactRaw []byte) error {
	if len(manifestRaw) == 0 || len(manifestRaw) > MaxRetainedArtifactManifestBytes || len(artifactRaw) == 0 || len(artifactRaw) > MaxRetainedArtifactBytes {
		return fmt.Errorf("%w: retained artifact exceeds its size bounds", ErrFederationIngestValidation)
	}
	return nil
}

// ValidateFederationEnrollmentRelayShape checks the relay columns of an
// enrollment row: a legacy grant carries no relay identity, parent credential
// or downstream flag, and a relay grant carries all of its binding identity,
// parent credential and project. Native creation, JSONL import and federation
// replay all apply it, since the schema no longer does.
func ValidateFederationEnrollmentRelayShape(protocolVersion int, bindingUID *string, parentTokenID, projectID *int64, resetEpoch int64, serveDownstream bool) error {
	if resetEpoch <= 0 {
		return errors.New("relay reset epoch must be positive")
	}
	switch protocolVersion {
	case 0:
		if bindingUID != nil || parentTokenID != nil || serveDownstream {
			return errors.New("a legacy enrollment carries no relay binding, parent credential or downstream flag")
		}
	case RelayProtocolVersion:
		if bindingUID == nil || !uid.Valid(*bindingUID) || parentTokenID == nil || projectID == nil {
			return errors.New("a relay enrollment requires a binding identity, parent credential and project")
		}
	default:
		return fmt.Errorf("unsupported relay protocol version %d", protocolVersion)
	}
	return nil
}
