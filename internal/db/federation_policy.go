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

// PreparedFederationEnrollment preserves whether the caller supplied the
// plaintext token while carrying normalized storage parameters.
type PreparedFederationEnrollment struct {
	Params        CreateFederationEnrollmentParams
	ExplicitToken bool
}

// PrepareFederationEnrollmentParams generates missing secret material and
// normalizes the backend-neutral enrollment contract before any transaction
// mutates federation state.
func PrepareFederationEnrollmentParams(
	input CreateFederationEnrollmentParams,
) (PreparedFederationEnrollment, error) {
	explicitToken := input.Token != ""
	if input.Token == "" {
		token, err := NewFederationToken()
		if err != nil {
			return PreparedFederationEnrollment{}, err
		}
		input.Token = token
	}
	if !katauid.Valid(input.SpokeInstanceUID) {
		return PreparedFederationEnrollment{}, fmt.Errorf(
			"invalid spoke instance uid %q", input.SpokeInstanceUID,
		)
	}
	capabilities, err := CanonicalFederationCapabilities(input.Capabilities)
	if err != nil {
		return PreparedFederationEnrollment{}, err
	}
	input.Capabilities = capabilities
	input.Actor = strings.TrimSpace(input.Actor)
	if err := ValidateTokenActor(input.Actor); err != nil {
		return PreparedFederationEnrollment{}, fmt.Errorf("federation enrollment actor: %w", err)
	}
	if input.AllowAdoptionSnapshotAuthors && input.ProjectID == nil {
		return PreparedFederationEnrollment{}, fmt.Errorf(
			"allow adoption snapshot authors requires project-scoped enrollment",
		)
	}
	return PreparedFederationEnrollment{Params: input, ExplicitToken: explicitToken}, nil
}

// FederationEnrollmentMatchesCreate reports whether an active stored grant
// is the exact normalized grant requested by an enrollment create.
func FederationEnrollmentMatchesCreate(
	enrollment FederationEnrollment,
	input CreateFederationEnrollmentParams,
) bool {
	return enrollment.RevokedAt == nil &&
		enrollment.SpokeInstanceUID == input.SpokeInstanceUID &&
		sameOptionalInt64(enrollment.ProjectID, input.ProjectID) &&
		enrollment.Capabilities == input.Capabilities &&
		enrollment.Actor == input.Actor &&
		enrollment.AllowAdoptionSnapshotAuthors == input.AllowAdoptionSnapshotAuthors
}

func sameOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
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
