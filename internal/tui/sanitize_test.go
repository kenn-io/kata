package tui

import (
	"regexp"
	"strings"
	"testing"
)

// assertSafeRender fails if rendered output contains raw ESC or CR
// bytes — both let agent-supplied text attack the terminal (cursor
// manipulation, column overwrite). Every TUI view that surfaces
// external content must sanitize before render.
// sgrSequence matches the styling escapes (CSI ... m) that Lip Gloss
// v2 styles legitimately emit at render time (profile downsampling
// moved into Bubble Tea's compositor, so rendered strings always carry
// SGR). Everything else — OSC title hijacks, CSI erase/cursor motion,
// bare ESC — must never survive input sanitization.
var sgrSequence = regexp.MustCompile("\x1b\\[[0-9;]*m")

func assertSafeRender(t testing.TB, got string) {
	t.Helper()
	got = sgrSequence.ReplaceAllString(got, "")
	if strings.Contains(got, "\x1b") {
		t.Fatalf("non-SGR ESC survived in render: %q", got)
	}
	if strings.Contains(got, "\r") {
		t.Fatalf("CR survived in render: %q", got)
	}
}

// TestSanitizeForDisplay covers every input class sanitizeForDisplay
// must handle: ANSI/OSC escape sequences (multiple terminator forms),
// bare control bytes, Cf format runes (bidi override, zero-width
// joiners), and the legitimate-content cases that must pass through
// untouched.
func TestSanitizeForDisplay(t *testing.T) {
	rlo := string(rune(0x202E))  // RIGHT-TO-LEFT OVERRIDE
	zwsp := string(rune(0x200B)) // ZERO WIDTH SPACE
	zwnj := string(rune(0x200C)) // ZERO WIDTH NON-JOINER
	zwj := string(rune(0x200D))  // ZERO WIDTH JOINER

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "strips CSI color sequences",
			input: "\x1b[31mDANGER\x1b[0m fix login",
			want:  "DANGER fix login",
		},
		{
			name:  "strips OSC terminated by BEL (set window title)",
			input: "before\x1b]0;evil title\x07after",
			want:  "beforeafter",
		},
		{
			name:  "strips OSC terminated by ST (hyperlink)",
			input: "x\x1b]8;;file:///etc/passwd\x1b\\link\x1b]8;;\x1b\\y",
			want:  "xlinky",
		},
		{
			name:  "preserves newline and tab",
			input: "line one\nline two\tindented",
			want:  "line one\nline two\tindented",
		},
		{
			name:  "strips bare carriage return",
			input: "real\rINJECTED",
			want:  "realINJECTED",
		},
		{
			name:  "strips bare ESC",
			input: "be\x1bfore",
			want:  "before",
		},
		{
			name:  "no-op for plain ASCII",
			input: "fix login bug on Safari",
			want:  "fix login bug on Safari",
		},
		{
			name:  "preserves unicode (CJK, emoji, accented)",
			input: "修复 login 🐛 résumé",
			want:  "修复 login 🐛 résumé",
		},
		{
			// U+202E is category Cf (Format), not C (Control), so
			// unicode.IsControl alone wouldn't catch it. Constructed
			// from rune() to avoid embedding a Trojan-Source rune in
			// this file's literal text.
			name:  "strips U+202E bidi override",
			input: "fix " + rlo + "txetnoc lacitirc",
			want:  "fix txetnoc lacitirc",
		},
		{
			// Zero-width format runes are invisible but interfere
			// with search and copy-paste; strip them too.
			name:  "strips zero-width format runes",
			input: "spo" + zwsp + "of" + zwnj + "er" + zwj + "ed",
			want:  "spoofered",
		},
		{
			name:  "empty input short-circuits",
			input: "",
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeForDisplay(tc.input); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestListView_SanitizesMaliciousTitle: an issue title with embedded
// ANSI escapes must not reach the rendered list view. Regression for
// the sanitize-at-render boundary in buildRows.
func TestListView_SanitizesMaliciousTitle(t *testing.T) {
	lm := newListModel()
	lm.loading = false
	lm.issues = []Issue{
		{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "\x1b]0;HIJACK\x07normal title", Status: "open"},
	}
	out := lm.View(120, 30, viewChrome{})
	assertSafeRender(t, out)
	if strings.Contains(out, "HIJACK") {
		t.Fatalf("OSC payload reached rendered list: %q", out)
	}
	if !strings.Contains(out, "normal title") {
		t.Fatalf("legitimate title content dropped: %q", out)
	}
}

// TestDetailView_SanitizesMaliciousBody: an issue body containing a
// CSI sequence must be stripped before reaching the body window.
func TestDetailView_SanitizesMaliciousBody(t *testing.T) {
	dm := detailModel{
		issue: &Issue{
			UID: "01TEST-42aa", ShortID: "42aa", Title: "x", Status: "open",
			Body: "first line\n\x1b[2Joverwrite-attack\nthird",
		},
	}
	out := dm.View(120, 30, viewChrome{})
	assertSafeRender(t, out)
	if !strings.Contains(out, "overwrite-attack") {
		t.Fatalf("body text dropped (CSI strip should leave the payload): %q", out)
	}
}

// TestCommentsTab_SanitizesMaliciousAuthorAndBody: comment author and
// body are agent-supplied; both render paths must sanitize.
func TestCommentsTab_SanitizesMaliciousAuthorAndBody(t *testing.T) {
	cs := []CommentEntry{{
		ID: 1, Author: "alice\x1b[31m",
		Body: "body line\rOVERWRITE",
	}}
	out := renderCommentsTab(cs, 120, 20, 0, tabState{})
	assertSafeRender(t, out)
}
