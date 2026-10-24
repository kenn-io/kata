package cron

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestTextWidthAndSanitation(t *testing.T) {
	for _, c := range []struct {
		s    string
		w    int
		want string
	}{{"abcdef", 4, "abc…"}, {"abc", 5, "abc  "}, {"你好世界", 5, "你好…"}, {"e\u0301xyz", 2, "e\u0301…"}, {"abc", 0, ""}, {"abc", 1, "…"}, {"\x1b]8;;https://example.test\x1b\\abc\x1b]8;;\x1b\\", 4, "abc "}, {"a\nb\x1b[31mc", 6, "a bc  "}} {
		if got := fitCell(c.s, c.w); got != c.want {
			t.Errorf("Fit(%q,%d)=%q want %q", c.s, c.w, got, c.want)
		}
	}
	if got := pad("abcdef", 2); got != "abcdef" {
		t.Fatal(got)
	}
	if got := truncate("\x1b[31mabcdef", 3); got != "ab…" {
		t.Fatal(got)
	}
}
func TestWrap(t *testing.T) {
	if got := wrapText("alpha beta\ngamma", 6); !reflect.DeepEqual(got, []string{"alpha", "beta", "gamma"}) {
		t.Fatal(got)
	}
	for _, s := range wrapText("你好世界 abcdefghij\x1b[31m", 3) {
		if ansi.StringWidth(s) > 3 || strings.Contains(s, "\x1b") {
			t.Fatal(s)
		}
	}
	if got := wrapMax("one\ntwo\nthree", 20, 2); !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatal(got)
	}
	p := promptPreview("1\n2\n3\n4\n5\n6\n7\n8", 0, 100)
	if !reflect.DeepEqual(p, []string{"1", "2", "3", "4", "5", "6", "… 2 more lines"}) {
		t.Fatal(p)
	}
	if got := promptPreview(strings.Repeat("a", 100), 1, 120); ansi.StringWidth(got[0]) != 96 {
		t.Fatal(got)
	}
}
func TestTimeFormats(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{{time.Second, "1s ago"}, {time.Minute, "1m ago"}, {time.Hour, "1h ago"}, {24 * time.Hour, "1d ago"}} {
		if got := ago(now, now.Add(-tc.age)); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
	if ago(now, time.Time{}) != "" || stamp(time.Time{}) != "" {
		t.Fatal("zero timestamp displayed")
	}
	if dur(1500*time.Millisecond) != "2s" || stamp(now) != "2026-10-16 12:00:00" {
		t.Fatal("duration or timestamp formatting")
	}
}
