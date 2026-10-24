package cron

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kit/tui/termtext"
)

func plain(s string) string               { return termtext.SanitizeLine(s) }
func truncate(s string, width int) string { return termtext.Truncate(plain(s), width, "…") }
func pad(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = plain(s)
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}
func fitCell(s string, width int) string { return pad(truncate(s, width), width) }

func wrapText(s string, width int) []string { return termtext.Wrap(termtext.SanitizeBlock(s), width) }
func wrapMax(s string, width, maxLines int) []string {
	lines := wrapText(s, width)
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	return lines
}
func promptPreview(s string, maxLines, width int) []string {
	if maxLines <= 0 {
		maxLines = 6
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, min(len(lines), maxLines)+1)
	for _, line := range lines[:min(len(lines), maxLines)] {
		out = append(out, truncate(line, min(width, 96)))
	}
	if len(lines) > maxLines {
		out = append(out, fmt.Sprintf("… %d more lines", len(lines)-maxLines))
	}
	return out
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := max(time.Duration(0), now.Sub(t))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}
func dur(d time.Duration) string { return d.Round(time.Second).String() }

func clean(s string) string { return termtext.SanitizeLine(s) }
