package cron

import (
	"math"

	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kit/tui/termtext"
)

type caretPosition struct{ row, col int }
type visualText struct {
	lines     []string
	positions []caretPosition
}

// layoutText maps rune boundaries to the same display columns used for drawing.
func layoutText(text string, cols int) visualText {
	cols = max(1, cols)
	runes := []rune(text)
	v := visualText{lines: []string{""}, positions: make([]caretPosition, len(runes)+1)}
	row, col := 0, 0
	for i, r := range runes {
		if r == '\n' {
			v.positions[i] = caretPosition{row, col}
			if col >= cols {
				v.positions[i] = caretPosition{row + 1, 0}
			}
			v.lines = append(v.lines, "")
			row++
			col = 0
			continue
		}
		s := termtext.SanitizeLine(string(r))
		w := ansi.StringWidth(s)
		if col+w > cols && col > 0 {
			v.lines = append(v.lines, "")
			row++
			col = 0
		}
		v.positions[i] = caretPosition{row, col}
		if col >= cols {
			v.positions[i] = caretPosition{row + 1, 0}
		}
		v.lines[row] += s
		col += w
	}
	if col >= cols {
		v.lines = append(v.lines, "")
		row++
		col = 0
	}
	v.positions[len(runes)] = caretPosition{row, col}
	return v
}
func verticalCaret(text string, cols, caret, direction, preferred int) int {
	v := layoutText(text, cols)
	caret = max(0, min(caret, len(v.positions)-1))
	current := v.positions[caret]
	if preferred < 0 {
		preferred = current.col
	}
	target := max(0, min(len(v.lines)-1, current.row+direction))
	best, distance := caret, math.MaxInt
	for i, p := range v.positions {
		if p.row != target {
			continue
		}
		d := p.col - preferred
		if d < 0 {
			d = -d
		}
		if d < distance || d == distance && i > best {
			best, distance = i, d
		}
	}
	return best
}
func (m *jobForm) textareaCols() int { return max(1, min(m.width, max(10, m.width-6))) }
func (m *jobForm) moveCaret(key string) {
	runes := []rune(m.buffer)
	m.caret = max(0, min(m.caret, len(runes)))
	switch {
	case matchesKey(key, keys.Left):
		m.caret = max(0, m.caret-1)
	case matchesKey(key, keys.Right):
		m.caret = min(len(runes), m.caret+1)
	case matchesKey(key, keys.Home, keys.LineHome):
		m.caret = hardLineStart(runes, m.caret)
	case matchesKey(key, keys.End, keys.LineEnd):
		for m.caret < len(runes) && runes[m.caret] != '\n' {
			m.caret++
		}
	case matchesKey(key, keys.DocHome):
		m.caret = 0
	case matchesKey(key, keys.DocEnd):
		m.caret = len(runes)
	case matchesKey(key, keys.ScrollUp, keys.ScrollDown):
		v := layoutText(m.buffer, m.textareaCols())
		if m.preferredCol < 0 {
			m.preferredCol = v.positions[m.caret].col
		}
		direction := 1
		if keys.ScrollUp.matchesString(key) {
			direction = -1
		}
		m.caret = verticalCaret(m.buffer, m.textareaCols(), m.caret, direction, m.preferredCol)
		return
	}
	m.preferredCol = -1
}
func hardLineStart(runes []rune, caret int) int {
	for caret > 0 && runes[caret-1] != '\n' {
		caret--
	}
	return caret
}

func (v visualText) withCaret(caret, cols int) ([]string, int) {
	lines := append([]string(nil), v.lines...)
	p := v.positions[max(0, min(caret, len(v.positions)-1))]
	if p.col >= cols {
		lines = append(lines[:p.row+1], append([]string{"▏"}, lines[p.row+1:]...)...)
		return lines, p.row + 1
	}
	text := lines[p.row]
	lines[p.row] = ansi.Cut(text, 0, p.col) + "▏" + ansi.Cut(text, p.col, ansi.StringWidth(text))
	return lines, p.row
}
