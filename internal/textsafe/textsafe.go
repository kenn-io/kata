// Package textsafe sanitizes user-authored text before it is rendered
// to a terminal. The CLI and TUI both consume agent-generated content
// (issue titles, bodies, comments, author / owner fields, label names)
// from the daemon. Without sanitization, a malicious or accidental
// title containing ANSI escape sequences could clear the screen, set
// the window title, hyperlink to an attacker-controlled URL, or paint
// over surrounding rows. Embedded control characters (carriage
// returns, vertical tabs, bidi overrides) can similarly reorder or
// hide content.
//
// JSON output paths must NOT use this package — agents and downstream
// tools need the daemon's raw bytes. Sanitization belongs at the
// human-facing terminal boundary only.
package textsafe

import (
	"regexp"
	"strings"
	"unicode"
)

// ansiEscapePattern matches the two ANSI escape families an agent-
// authored string could realistically reach a terminal with:
//
//   - CSI: ESC [ ... <final byte> — colors, cursor moves, scroll regions.
//   - OSC: ESC ] ... (BEL | ESC \) — title sets, hyperlinks, palette.
//
// Other escape families (DCS, SOS, PM, APC) are rare in practice and
// the OSC alternation here would not catch them, but the IsControl-
// strip below removes their introducer (ESC) so they cannot
// initiate. Stripping the introducer alone leaves the payload as
// plain text in the buffer, which is the conservative outcome.
var ansiEscapePattern = regexp.MustCompile(
	`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\]([^\x07\x1b]|\x1b[^\\])*(\x07|\x1b\\)`,
)

// StripANSI removes only the ANSI escape sequences from s, leaving
// every other rune (including newlines, tabs, and Unicode control
// characters) intact. Use for width-measurement helpers that need
// to count visible cells in already-styled output — sanitization
// (Block / Line) would mangle the content. Not for input rendering.
func StripANSI(s string) string {
	return ansiEscapePattern.ReplaceAllString(s, "")
}

// Block sanitizes s for multi-line terminal output. Strips ANSI
// escape sequences and Unicode control characters so agent-authored
// text cannot push the terminal into an arbitrary state. Newlines
// and tabs survive because issue bodies and comments legitimately
// contain them; every other control rune is dropped.
//
// Cf (Unicode "Format" category) is dropped explicitly because
// unicode.IsControl returns false for it — but Cf includes runes
// like U+202E RIGHT-TO-LEFT OVERRIDE that can spoof or visually
// reorder agent-authored text in the TUI even after ANSI/control
// stripping. Treat them as untrusted alongside the C0/C1 controls.
//
// Apply at every render-time boundary that touches user-supplied
// text destined for the human-facing terminal: titles, bodies,
// comment bodies, authors, owners, event payloads. Daemon-generated
// structural tokens (status keywords, timestamps, issue numbers)
// are trusted and don't need this.
func Block(s string) string {
	if s == "" {
		return s
	}
	s = ansiEscapePattern.ReplaceAllString(s, "")
	if !strings.ContainsFunc(s, IsUnsafeTerminalRune) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !IsUnsafeTerminalRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Line sanitizes s for single-line terminal output. Same control /
// ANSI stripping as Block, plus replaces every embedded newline with
// the literal escape sequence "\n" so the rendered output stays on
// a single visual row. Use for list rows, owner / author cells, and
// any other context where a newline in the source would break the
// caller's row layout.
//
// Tabs are replaced with a single space (most terminals render tabs
// at variable widths that would break column alignment) and carriage
// returns are dropped (\r in a single-row context can rewind the
// cursor and overwrite the rest of the row).
func Line(s string) string {
	if s == "" {
		return s
	}
	s = Block(s) // strip controls/ANSI/Cf first
	if !strings.ContainsAny(s, "\n\t") {
		return s
	}
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", " ")
	return s
}

// IsUnsafeTerminalRune reports whether r is a control character or
// invisible Unicode format rune that should be stripped. Newline
// and tab survive (the caller decides per-context — Line replaces
// them, Block keeps them); everything else is removed.
func IsUnsafeTerminalRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}
