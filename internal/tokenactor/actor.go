// Package tokenactor validates the actor attributed to a bearer credential.
package tokenactor

import (
	"fmt"
	"strings"
)

// Bootstrap is reserved for administrator operations, not an ordinary token.
const Bootstrap = "bootstrap"

// Validate rejects empty actors and reserved bootstrap spellings.
func Validate(actor string) error {
	trimmed := strings.TrimSpace(actor)
	if trimmed == "" {
		return fmt.Errorf("actor must be non-empty")
	}
	if strings.EqualFold(trimmed, Bootstrap) {
		return fmt.Errorf("actor %q is reserved", Bootstrap)
	}
	return nil
}
