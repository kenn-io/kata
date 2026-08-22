package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestLinksChangedDesc covers the TUI describer for the aggregated
// issue.links_changed event from the PATCH (`kata edit`) path. Without
// a typed describer the events tab fell back to the raw "links_changed"
// fragment and dropped the actual add/remove detail (kata#1 follow-up).
func TestLinksChangedDesc(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name: "parent_set_only",
			payload: map[string]any{
				"parent_set": "aa10",
			},
			want: "links: +parent #aa10",
		},
		{
			name: "parent_removed_only",
			payload: map[string]any{
				"parent_removed": "bb07",
			},
			want: "links: -parent #bb07",
		},
		{
			name: "parent_replace",
			payload: map[string]any{
				"parent_set":     "aa10",
				"parent_removed": "bb07",
			},
			want: "links: parent #bb07→#aa10",
		},
		{
			name: "blocks_added",
			payload: map[string]any{
				"blocks_added": []any{"x050", "x051"},
			},
			want: "links: +blocks #x050, +blocks #x051",
		},
		{
			name: "blocked_by_removed",
			payload: map[string]any{
				"blocked_by_removed": []any{"y015"},
			},
			want: "links: -blocked_by #y015",
		},
		{
			name: "mixed",
			payload: map[string]any{
				"parent_set":      "p002",
				"blocks_added":    []any{"x050"},
				"related_removed": []any{"r009"},
			},
			want: "links: +parent #p002, +blocks #x050, -related #r009",
		},
		{
			name:    "empty",
			payload: map[string]any{},
			want:    "links unchanged",
		},
		{
			name:    "nil_payload",
			payload: nil,
			want:    "links changed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := EventLogEntry{Type: "issue.links_changed", Payload: tc.payload}
			got := eventDescription(e)
			assert.Equal(t, tc.want, got)
		})
	}
}
