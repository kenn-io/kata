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

func TestEventChunkKeepsMultilineDeleteReasonOnOneRow(t *testing.T) {
	e := EventLogEntry{Type: "issue.soft_deleted", Actor: "reviewer", Payload: map[string]any{
		"deleted_at": "2026-09-15T01:02:03Z", "reason": "duplicate of the import task\nsee the thread",
	}}
	lines := eventChunkLines(e, 80, false, nil)
	assert.Len(t, lines, 1)
	assert.NotContains(t, lines[0], "\n")
	assert.Contains(t, lines[0], "see the thread")
}
