package federation

import (
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// Capabilities is the canonical API value plus the human-facing display
// value for a federation capability set.
type Capabilities struct {
	API     string
	Display string
}

// NormalizeCapabilities maps user-facing federation capabilities to the
// canonical API/storage representation and the deterministic display value.
func NormalizeCapabilities(raw string) (Capabilities, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "pull,push,lease"
	}
	parts := strings.Split(raw, ",")
	mapped := make([]string, 0, len(parts))
	for _, part := range parts {
		capability := strings.TrimSpace(part)
		if capability == "lease" {
			capability = "claim"
		}
		if !db.IsSupportedFederationCapability(capability) {
			return Capabilities{}, fmt.Errorf("unknown federation capability %q", strings.TrimSpace(part))
		}
		mapped = append(mapped, capability)
	}
	apiCaps, err := db.CanonicalFederationCapabilities(strings.Join(mapped, ","))
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{API: apiCaps, Display: DisplayCapabilities(apiCaps)}, nil
}

// DisplayCapabilities converts API/storage capabilities to the TUI/CLI
// display spelling, including claim -> lease.
func DisplayCapabilities(apiCaps string) string {
	parts := strings.Split(apiCaps, ",")
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		capability := strings.TrimSpace(part)
		if capability == "" {
			continue
		}
		if capability == "claim" {
			capability = "lease"
		}
		seen[capability] = true
	}
	out := make([]string, 0, len(parts))
	for _, capability := range []string{"pull", "push", "lease"} {
		if seen[capability] {
			out = append(out, capability)
		}
	}
	return strings.Join(out, ",")
}
