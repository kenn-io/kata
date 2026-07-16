package db

import "strings"

// NormalizeCloseMessage canonicalizes close prose for duplicate-message
// detection without changing the persisted event payload.
func NormalizeCloseMessage(message string) string {
	message = strings.TrimSpace(message)
	message = strings.Join(strings.Fields(message), " ")
	message = strings.ToLower(message)
	return strings.TrimRight(message, ".?!")
}
