package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"hegel.dev/go/hegel"
)

// The event header consumes the canonical teammate, including creation/comment
// identity already resolved by the API. Raw payload attribution is not a fallback.
func TestEventChunkLines_TeammateLabel(t *testing.T) {
	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, actor, teammate, want string
	}{
		{"tagged", "alice", "claude.opus-5-5", "alice / claude.opus-5-5"},
		{"legacy", "alice", "", "alice"},
		{"minimal_handle", "alice", "0", "alice / 0"},
		{"maximum_handle", "alice", strings.Repeat("A", 64), "alice / " + strings.Repeat("A", 64)},
		{"actor_ansi", "\x1b[31malice\x1b[0m", "codex", "alice / codex"},
		{"teammate_ansi", "alice", "\x1b[31mcodex\x1b[0m", "alice / codex"},
		{"actor_osc", "\x1b]0;spoof\x07alice", "codex", "alice / codex"},
		{"teammate_osc", "alice", "\x1b]0;spoof\x1b\\codex", "alice / codex"},
		{"actor_controls", "al\x00i\rc\u202ee", "codex", "alice / codex"},
		{"teammate_controls", "alice", "co\x00d\re\u202ex", "alice / codex"},
		{"legacy_hostile_actor", "\x1b[31malice\x1b[0m\u202e", "", "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := eventChunkLines(EventLogEntry{
				Type: "issue.closed", CreatedAt: when, Actor: tc.actor, Teammate: tc.teammate,
				Payload: map[string]any{"reason": "done", "teammate": "payload-worker"},
			}, 0, false)
			assert.Equal(t, []string{"  [issue.closed] 2026-08-01 12:00 " + tc.want + " — closed (done)"}, got)
		})
	}
}

// Retain the minimal counterexample from the test-first property run.
func TestEventChunkLines_TeammateWithEmptyActor(t *testing.T) {
	got := eventChunkLines(EventLogEntry{
		Type: "issue.created", Teammate: "A",
		CreatedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
	}, 0, false)
	assert.Equal(t, []string{"  [issue.created] 2026-08-01 12:00  / A — created"}, got)
}

// Every valid portable handle (and absence) must survive verbatim in the label.
// The oracle uses generated printable inputs and literal descriptions, without
// calling the production attribution, description or sanitization helpers.
func TestPropertyEventChunkLines_TeammateLabel(t *testing.T) {
	handle := hegel.FromRegex(`([A-Za-z0-9][A-Za-z0-9._-]{0,63})?`, true)
	kinds := []struct{ kind, description string }{
		{"issue.created", "created"},
		{"issue.commented", "added comment"},
		{"issue.closed", "closed"},
		{"issue.moved", "moved"},
		{"issue.future_action", "future_action"},
	}
	hegel.Test(t, func(ht *hegel.T) {
		actor := hegel.Draw(ht, hegel.Text().MinCodepoint(' ').MaxCodepoint('~'))
		teammate := hegel.Draw(ht, handle)
		kind := hegel.Draw(ht, hegel.SampledFrom(kinds))
		cursor := hegel.Draw(ht, hegel.Booleans())
		label := actor
		if teammate != "" {
			label += " / " + teammate
		}
		prefix := "  "
		if cursor {
			prefix = "> "
		}
		want := prefix + "[" + kind.kind + "] 2026-08-01 12:00 " + label + " — " + kind.description
		got := eventChunkLines(EventLogEntry{
			Type: kind.kind, Actor: actor, Teammate: teammate,
			CreatedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
			Payload:   map[string]any{"teammate": "payload-worker", "author": "payload-author", "metadata": map[string]any{"teammate": "creation-worker"}},
		}, 0, cursor)
		if len(got) != 1 || got[0] != want {
			ht.Fatalf("event header = %q; want %q (actor=%q teammate=%q)", got, want, actor, teammate)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
