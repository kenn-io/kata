package cron

import (
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const marqueeGap = "   ·   "
const marqueeHold = 6

// marqueePart retains the source rune index for segmented inspector styling.
type marqueePart struct {
	text  string
	index int
}

func marqueeWindow(s string, width, frame int) []marqueePart {
	if width <= 0 {
		return nil
	}
	runes := []rune(s)
	loop := []rune(s + marqueeGap)
	overflow := ansi.StringWidth(s) > width
	offset := 0
	if overflow {
		offset = max(0, frame%(len(loop)+marqueeHold)-marqueeHold)
	}
	var out []marqueePart
	used := 0
	for i := 0; i < (width+1)*len(loop); i++ {
		index := offset + i
		if overflow {
			index %= len(loop)
		} else if index >= len(runes) {
			break
		}
		ch := loop[index]
		w := ansi.StringWidth(string(ch))
		if used+w > width {
			break
		}
		if used == 0 && unicode.IsMark(ch) {
			continue
		}
		if used == width && w > 0 {
			break
		}
		source := index
		if index >= len(runes) {
			source = -1
		}
		out = append(out, marqueePart{string(ch), source})
		used += w
	}
	if used < width {
		out = append(out, marqueePart{strings.Repeat(" ", width-used), -1})
	}
	return out
}
func marquee(s string, width, frame int) string {
	var out strings.Builder
	for _, part := range marqueeWindow(plain(s), width, frame) {
		out.WriteString(part.text)
	}
	return out.String()
}

type marqueeTickMsg struct{ generation uint64 }

func marqueeTick(generation uint64) tea.Cmd {
	return tea.Tick(320*time.Millisecond, func(time.Time) tea.Msg { return marqueeTickMsg{generation: generation} })
}
func (m *model) updateMarquee() tea.Cmd {
	if !m.active {
		return nil
	}
	frame := m.renderFrame()
	if !frame.Animates {
		if m.marqueePending {
			m.marqueePending = false
			m.marqueeGeneration++
		}
		return nil
	}
	if m.marqueePending {
		return nil
	}
	m.marqueePending = true
	return marqueeTick(m.marqueeGeneration)
}

// marqueeState is owned by this view.
type marqueeState struct {
	marqueePending    bool
	marqueeGeneration uint64
	frame             int
}
