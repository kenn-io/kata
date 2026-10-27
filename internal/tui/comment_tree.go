package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// threadOrder groups replies under their locally visible targets, preserving
// input order among roots and siblings. Malformed cycles are retained once.
func threadOrder(comments []CommentEntry) []CommentEntry {
	byUID := make(map[string]int, len(comments))
	for i, c := range comments {
		if c.UID != "" {
			byUID[c.UID] = i
		}
	}
	children := make([][]int, len(comments))
	roots := make([]int, 0, len(comments))
	for i, c := range comments {
		parent, local := 0, false
		if c.Reply != nil && c.Reply.UID != "" {
			parent, local = byUID[c.Reply.UID]
		}
		if local {
			children[parent] = append(children[parent], i)
		} else {
			roots = append(roots, i)
		}
	}
	ordered := make([]CommentEntry, 0, len(comments))
	visited := make([]bool, len(comments))
	var visit func(int)
	visit = func(i int) {
		if visited[i] {
			return
		}
		visited[i] = true
		ordered = append(ordered, comments[i])
		for _, child := range children[i] {
			visit(child)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	for i := range comments {
		visit(i)
	}
	return ordered
}

// commentFlagTags reports only direct confirmation and refutation evidence.
func commentFlagTags(c CommentEntry) string {
	return strings.Join(commentFlagTagParts(c), " ")
}

func commentFlagTagParts(c CommentEntry) []string {
	var confirmed, refuted, freshConfirm, freshRefute bool
	for _, link := range c.Backlinks {
		switch link.Kind {
		case "confirm":
			confirmed = true
			freshConfirm = freshConfirm || !link.TargetEdited
		case "refute":
			refuted = true
			freshRefute = freshRefute || !link.TargetEdited
		}
	}
	confirmGlyph, refuteGlyph := "✓", "✗"
	if activeColorMode == colorNone {
		confirmGlyph, refuteGlyph = "+", "x"
	}
	var tags []string
	if confirmed {
		tag := confirmGlyph + " confirm"
		if !freshConfirm {
			tag += " (before edit)"
		}
		tags = append(tags, openStyle.Render(tag))
	}
	if refuted {
		tag := refuteGlyph + " refute"
		if !freshRefute {
			tag += " (before edit)"
		}
		tags = append(tags, errorStyle.Render(tag))
	}
	return tags
}

func commentKindGlyph(kind string) string {
	glyph := "─"
	if activeColorMode == colorNone {
		glyph = "-"
	}
	switch kind {
	case "confirm":
		if activeColorMode == colorNone {
			glyph = "+"
		} else {
			glyph = "✓"
		}
		return openStyle.Render(glyph)
	case "refute":
		if activeColorMode == colorNone {
			glyph = "x"
		} else {
			glyph = "✗"
		}
		return errorStyle.Render(glyph)
	case "supersede":
		if activeColorMode == colorNone {
			glyph = "^"
		} else {
			glyph = "↻"
		}
		return closedStyle.Render(glyph)
	default:
		return subtleStyle.Render(glyph)
	}
}

// wrapCommentHeader preserves the two-space field boundaries when possible.
func wrapCommentHeader(header, rail, continuation string, width int, cursor bool) []string {
	fields := strings.Split(header, "  ")
	prefix := "  "
	if cursor {
		prefix = "> "
	}
	line := prefix + rail
	hasField := false
	var lines []string
	for _, field := range fields {
		if field == "" {
			continue
		}
		separator := ""
		if hasField {
			separator = "  "
		}
		if ansi.StringWidth(line+separator+field) > width && separator != "" {
			lines = append(lines, line)
			line = continuation
			separator = ""
		}
		budget := max(1, width-ansi.StringWidth(continuation))
		pieces := strings.Split(ansi.Hardwrap(field, budget, true), "\n")
		line += separator + pieces[0]
		hasField = true
		for _, piece := range pieces[1:] {
			lines = append(lines, line)
			line = continuation + piece
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}
