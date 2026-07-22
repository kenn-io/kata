package db

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	katauid "go.kenn.io/kata/internal/uid"
)

// NewFederationToken returns a cryptographically random enrollment secret.
func NewFederationToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate federation token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

// PrepareFederationEnrollmentParams generates missing secret material and
// normalizes the backend-neutral enrollment contract before any transaction
// mutates federation state.
func PrepareFederationEnrollmentParams(
	input CreateFederationEnrollmentParams,
) (CreateFederationEnrollmentParams, error) {
	if input.Token == "" {
		token, err := NewFederationToken()
		if err != nil {
			return CreateFederationEnrollmentParams{}, err
		}
		input.Token = token
	}
	if !katauid.Valid(input.SpokeInstanceUID) {
		return CreateFederationEnrollmentParams{}, fmt.Errorf(
			"invalid spoke instance uid %q", input.SpokeInstanceUID,
		)
	}
	capabilities, err := CanonicalFederationCapabilities(input.Capabilities)
	if err != nil {
		return CreateFederationEnrollmentParams{}, err
	}
	input.Capabilities = capabilities
	input.Actor = strings.TrimSpace(input.Actor)
	if err := ValidateTokenActor(input.Actor); err != nil {
		return CreateFederationEnrollmentParams{}, fmt.Errorf("federation enrollment actor: %w", err)
	}
	if input.AllowAdoptionSnapshotAuthors && input.ProjectID == nil {
		return CreateFederationEnrollmentParams{}, fmt.Errorf(
			"allow adoption snapshot authors requires project-scoped enrollment",
		)
	}
	return input, nil
}

// ValidateFederationQuarantine validates the portable poisoned-batch shape.
func ValidateFederationQuarantine(input RecordFederationQuarantineParams) error {
	if input.ProjectID <= 0 {
		return fmt.Errorf("federation quarantine project id is required")
	}
	if input.Direction != FederationQuarantineDirectionPush && input.Direction != FederationQuarantineDirectionPull {
		return fmt.Errorf("federation quarantine direction must be push or pull")
	}
	if input.FirstEventID < 0 || input.LastEventID < input.FirstEventID {
		return fmt.Errorf("federation quarantine event id range is invalid")
	}
	if strings.TrimSpace(input.Error) == "" {
		return fmt.Errorf("federation quarantine error is required")
	}
	return nil
}
