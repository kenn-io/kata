package db

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// FederationTokenHash returns the SHA-256 hex digest used for enrollment token
// lookup. Plaintext federation tokens must never be persisted.
func FederationTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CanonicalFederationCapabilities validates and normalizes supported
// capabilities as sorted, de-duplicated comma-separated text.
func CanonicalFederationCapabilities(raw string) (string, error) {
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		capability := strings.TrimSpace(part)
		if capability == "" {
			return "", fmt.Errorf("empty federation capability")
		}
		if !IsSupportedFederationCapability(capability) {
			return "", fmt.Errorf("unknown federation capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
	if len(seen) == 0 {
		return "", fmt.Errorf("empty federation capability")
	}
	out := make([]string, 0, len(seen))
	for capability := range seen {
		out = append(out, capability)
	}
	sort.Strings(out)
	return strings.Join(out, ","), nil
}

// IsSupportedFederationCapability reports whether capability is a known
// federation capability tag. Capability strings outside this set are rejected
// by enrollment creation and authorization.
func IsSupportedFederationCapability(capability string) bool {
	switch capability {
	case "claim", "pull", "push":
		return true
	default:
		return false
	}
}

// ValidateTokenActor rejects empty actors and reserved bootstrap spellings.
func ValidateTokenActor(actor string) error {
	trimmed := strings.TrimSpace(actor)
	if trimmed == "" {
		return fmt.Errorf("actor must be non-empty")
	}
	if strings.EqualFold(trimmed, BootstrapActor) {
		return fmt.Errorf("actor %q is reserved", BootstrapActor)
	}
	return nil
}
