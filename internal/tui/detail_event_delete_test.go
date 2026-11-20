package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEventDescriptionShowsDeleteReason(t *testing.T) {
	withReason := EventLogEntry{Type: "issue.soft_deleted", Payload: map[string]any{
		"deleted_at": "2026-09-15T01:02:03Z", "reason": "duplicate of the import task",
	}}
	assert.Equal(t, "deleted (duplicate of the import task)", eventDescription(withReason))

	without := EventLogEntry{Type: "issue.soft_deleted", Payload: map[string]any{"deleted_at": "2026-09-15T01:02:03Z"}}
	assert.Equal(t, "deleted", eventDescription(without))
}
