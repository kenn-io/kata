package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEventDescriptionOmitsDeleteReasonFromHeadRow(t *testing.T) {
	withReason := EventLogEntry{Type: "issue.soft_deleted", Payload: map[string]any{
		"deleted_at": "2026-09-15T01:02:03Z", "reason": "duplicate of the import task",
	}}
	assert.Equal(t, "deleted", eventDescription(withReason))
}

func TestEventChunkWrapsLongDeleteReasonOnNarrowPane(t *testing.T) {
	e := EventLogEntry{Type: "issue.soft_deleted", Actor: "reviewer", Payload: map[string]any{
		"deleted_at": "2026-09-15T01:02:03Z",
		"reason":     "duplicate of the import task tracked upstream, trailing-marker",
	}}
	lines := eventChunkLines(e, 40, false, nil)
	assert.Greater(t, len(lines), 2)
	assert.True(t, strings.HasSuffix(lines[0], "— deleted"), lines[0])
	assert.Contains(t, lines[1], "  reason: ")
	var tail strings.Builder
	for _, l := range lines[1:] {
		tail.WriteString(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "reason:")))
	}
	assert.True(t, strings.HasSuffix(tail.String(), "marker"), tail.String())
}

func TestEventChunkWithoutDeleteReasonIsSingleRow(t *testing.T) {
	e := EventLogEntry{Type: "issue.soft_deleted", Actor: "reviewer", Payload: map[string]any{
		"deleted_at": "2026-09-15T01:02:03Z",
	}}
	assert.Len(t, eventChunkLines(e, 40, false, nil), 1)
}

func TestEventChunkKeepsMultilineDeleteReasonOnOneRow(t *testing.T) {
	e := EventLogEntry{Type: "issue.soft_deleted", Actor: "reviewer", Payload: map[string]any{
		"deleted_at": "2026-09-15T01:02:03Z", "reason": "duplicate of the import task\nsee the thread",
	}}
	lines := eventChunkLines(e, 200, false, nil)
	assert.Len(t, lines, 2)
	for _, l := range lines {
		assert.NotContains(t, l, "\n")
	}
	assert.Contains(t, lines[1], "see the thread")
}
