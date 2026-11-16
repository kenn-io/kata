package tui

import (
	"strings"
	"testing"
	"uuid"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/metadata"
)

// TestEventChunkLines_CloseDetail covers the events-tab rendering for
// issue.closed events. Before this change the tab showed only
// "closed (done)" with no message or evidence; reviewers had to drop to
// `kata audit closes` to see what was actually closed.
func TestEventChunkLines_CloseDetail(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    []string
	}{
		{
			name: "message_and_commit",
			payload: map[string]any{
				"reason":  "done",
				"message": "Fixed Safari callback double-submit.",
				"evidence": []any{
					map[string]any{"type": "commit", "sha": "a1b2c3d"},
				},
			},
			want: []string{
				"  message: Fixed Safari callback double-submit.",
				"  evidence: commit a1b2c3d",
			},
		},
		{
			name: "every_evidence_type",
			payload: map[string]any{
				"reason":  "done",
				"message": "Closing.",
				"evidence": []any{
					map[string]any{"type": "commit", "sha": "abc1234"},
					map[string]any{"type": "pr", "url": "https://example.com/pr/1"},
					map[string]any{"type": "test", "command": "go test ./..."},
					map[string]any{"type": "reviewed-paths", "paths": []any{"a.go", "b.go"}},
					map[string]any{"type": "external", "account": "email thread archived"},
					map[string]any{"type": "no-change-audit", "rationale": "schema unchanged"},
					map[string]any{"type": "duplicate-of", "issue_ref": "kata#d4ex"},
					map[string]any{"type": "superseded-by", "issue_ref": "kata#sxyz"},
				},
			},
			want: []string{
				"  message: Closing.",
				"  evidence: commit abc1234",
				"  evidence: pr https://example.com/pr/1",
				"  evidence: test go test ./...",
				"  evidence: reviewed-paths a.go, b.go",
				"  evidence: external email thread archived",
				"  evidence: no-change-audit schema unchanged",
				"  evidence: duplicate-of kata#d4ex",
				"  evidence: superseded-by kata#sxyz",
			},
		},
		{
			name: "message_only_no_evidence",
			payload: map[string]any{
				"reason":  "wontfix",
				"message": "Not in scope for this milestone.",
			},
			want: []string{
				"  message: Not in scope for this milestone.",
			},
		},
		{
			name: "tui_bypass_no_message_no_evidence",
			payload: map[string]any{
				"reason": "done",
			},
			want: nil,
		},
		{
			name:    "nil_payload",
			payload: nil,
			want:    nil,
		},
		{
			name: "unknown_evidence_type_falls_back_to_tag",
			payload: map[string]any{
				"reason": "done",
				"evidence": []any{
					map[string]any{"type": "future-thing", "data": "ignored"},
				},
			},
			want: []string{
				"  evidence: future-thing",
			},
		},
		{
			name: "attachment_names_the_frozen_file",
			payload: map[string]any{
				"reason": "done",
				"evidence": []any{
					map[string]any{
						"type": "attachment", "attachment_id": "01KATTACH0000000000000REPT",
						"attachment": map[string]any{
							"name": "report.pdf", "media_type": "application/pdf", "size": float64(2048),
							"docbank_version": "6f1c2e8a-2222-4222-8222-222222222222",
						},
					},
					map[string]any{"type": "attachment", "attachment_id": "01KATTACH0000000000000LOGS"},
				},
			},
			want: []string{
				"  evidence: attachment 01KATTACH0000000000000REPT: report.pdf · 2048 bytes",
				"  evidence: attachment 01KATTACH0000000000000LOGS",
			},
		},
		{
			name: "malformed_evidence_array_dropped",
			payload: map[string]any{
				"reason":   "done",
				"evidence": "not-an-array",
			},
			want: nil,
		},
		{
			name: "non_map_evidence_items_skipped",
			payload: map[string]any{
				"reason": "done",
				"evidence": []any{
					"a string in the array",
					map[string]any{"type": "commit", "sha": "abc"},
				},
			},
			want: []string{
				"  evidence: commit abc",
			},
		},
		{
			// Regression for #19129: a multi-line message must stay on
			// one physical row (chunk windowing counts lines per chunk
			// element). Embedded \n is rendered as the literal escape
			// sequence so users see that more prose exists in the close
			// payload without breaking the events tab layout.
			name: "multiline_message_collapsed",
			payload: map[string]any{
				"reason":  "done",
				"message": "first line\nsecond line\nthird line",
			},
			want: []string{
				`  message: first line\nsecond line\nthird line`,
			},
		},
		{
			// Same regression for evidence values — a test command can
			// reasonably contain a heredoc with newlines.
			name: "multiline_test_command_collapsed",
			payload: map[string]any{
				"reason": "done",
				"evidence": []any{
					map[string]any{"type": "test", "command": "go test ./...\nrun: pass"},
				},
			},
			want: []string{
				`  evidence: test go test ./...\nrun: pass`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := EventLogEntry{Type: "issue.closed", Payload: tc.payload}
			// width=0 keeps the rows un-wrapped so these table cases
			// can assert the unwrapped shape. The wrapping behaviour
			// has its own dedicated test below.
			got := closeDetailLines(e, 0, nil)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestEventChunkLines_HeaderShape pins down the chunk structure for
// closed and non-closed events. The header line is always present;
// closed events get the close-detail tail, others don't.
func TestEventChunkLines_HeaderShape(t *testing.T) {
	t.Run("non_close_event_single_line", func(t *testing.T) {
		e := EventLogEntry{
			Type:    "issue.commented",
			Actor:   "wes",
			Payload: map[string]any{"comment_id": float64(7)},
		}
		lines := eventChunkLines(e, 0, false, nil)
		require.Len(t, lines, 1)
		assert.Contains(t, lines[0], "[issue.commented]")
		assert.Contains(t, lines[0], "wes")
		assert.Contains(t, lines[0], "added comment")
	})

	t.Run("close_event_with_detail_multi_line", func(t *testing.T) {
		e := EventLogEntry{
			Type:  "issue.closed",
			Actor: "wes",
			Payload: map[string]any{
				"reason":  "done",
				"message": "shipped",
				"evidence": []any{
					map[string]any{"type": "commit", "sha": "abc1234"},
				},
			},
		}
		lines := eventChunkLines(e, 0, true, nil)
		require.GreaterOrEqual(t, len(lines), 3)
		assert.Contains(t, lines[0], "> ", "first line carries cursor marker")
		assert.Contains(t, lines[0], "[issue.closed]")
		assert.Contains(t, lines[0], "closed (done)")
		assert.Equal(t, "  message: shipped", lines[1])
		assert.Equal(t, "  evidence: commit abc1234", lines[2])
	})

	t.Run("close_event_no_detail_single_line", func(t *testing.T) {
		e := EventLogEntry{
			Type:    "issue.closed",
			Actor:   "wes",
			Payload: map[string]any{"reason": "done"},
		}
		lines := eventChunkLines(e, 0, false, nil)
		require.Len(t, lines, 1)
		assert.Contains(t, lines[0], "closed (done)")
	})
}

// TestCloseDetailLines_Wrap pins down hanging-indent wrap behavior so a
// long close message stays fully visible on a narrow tab pane instead
// of getting clipped to "... rates and fall…". Reported by the user
// after the initial close-detail commit landed.
func TestCloseDetailLines_Wrap(t *testing.T) {
	e := EventLogEntry{
		Type: "issue.closed",
		Payload: map[string]any{
			"reason":  "done",
			"message": "FX fetchers now return a typed partial-failure error while preserving successful rates",
			"evidence": []any{
				map[string]any{"type": "commit", "sha": "359c7ceb"},
			},
		},
	}
	// Width 60 forces a wrap on the long message but keeps the
	// commit-evidence line single. Message prefix "  message: " is
	// 11 cells wide, so the budget per line is 49.
	lines := closeDetailLines(e, 60, nil)
	require.GreaterOrEqual(t, len(lines), 3, "message must wrap to at least 2 rows + evidence row")
	assert.True(t, strings.HasPrefix(lines[0], "  message: "), "first row carries the label")
	assert.True(t, strings.HasPrefix(lines[1], "           "),
		"continuation row uses an 11-space hanging indent under the value column")
	for i, ln := range lines {
		assert.LessOrEqual(t, runewidth.StringWidth(ln), 60,
			"row %d (%q) exceeds budgeted width", i, ln)
	}
	// Last line is the (un-wrapped) commit evidence.
	assert.Equal(t, "  evidence: commit 359c7ceb", lines[len(lines)-1])

	// Reassembling the wrapped message body must reconstruct the
	// original sanitized text — no characters lost to clipping.
	body := strings.TrimPrefix(lines[0], "  message: ")
	for _, ln := range lines[1 : len(lines)-1] {
		body += strings.TrimPrefix(ln, "           ")
	}
	assert.Equal(t,
		"FX fetchers now return a typed partial-failure error while preserving successful rates",
		body)
}

func TestCloseEvidenceComparesAttachmentsWithTheIssue(t *testing.T) {
	const same, other = "6f1c2e8a-2222-4222-8222-222222222222", "7a2d3f9b-3333-4333-8333-333333333333"
	frozen := func(version string) map[string]any {
		return map[string]any{"name": "report.pdf", "media_type": "application/pdf", "size": float64(2048), "docbank_version": version}
	}
	current := func(version string) map[string]metadata.Attachment {
		return map[string]metadata.Attachment{"R1": {Name: "report.pdf", MediaType: "application/pdf", Size: 2048, DocbankVersion: uuid.MustParse(version)}}
	}
	for _, tc := range []struct {
		name    string
		item    map[string]any
		current map[string]metadata.Attachment
		want    string
	}{
		{"unchanged", map[string]any{"attachment_id": "R1", "attachment": frozen(same)}, current(same), "attachment R1: report.pdf · 2048 bytes"},
		{"changed", map[string]any{"attachment_id": "R1", "attachment": frozen(same)}, current(other), "attachment R1: report.pdf · 2048 bytes (changed)"},
		{"removed", map[string]any{"attachment_id": "R1", "attachment": frozen(same)}, map[string]metadata.Attachment{}, "attachment R1: report.pdf · 2048 bytes (removed)"},
		{"issue not loaded", map[string]any{"attachment_id": "R1", "attachment": frozen(same)}, nil, "attachment R1: report.pdf · 2048 bytes"},
		{"invalid frozen details", map[string]any{"attachment_id": "R1", "attachment": map[string]any{"name": "a/b", "size": float64(1)}}, current(same), "Invalid attachment details: R1"},
		{"no descriptor, still attached", map[string]any{"attachment_id": "R1"}, current(same), "attachment R1: report.pdf · 2048 bytes"},
		{"no descriptor, removed", map[string]any{"attachment_id": "R1"}, map[string]metadata.Attachment{}, "attachment R1 (removed)"},
		{"no descriptor, issue not loaded", map[string]any{"attachment_id": "R1"}, nil, "attachment R1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.item["type"] = "attachment"
			assert.Equal(t, tc.want, formatEvidenceItem(tc.item, tc.current))
		})
	}
}
