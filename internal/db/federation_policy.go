package db

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// NewFederationToken returns a cryptographically random enrollment secret.
func NewFederationToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate federation token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
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
