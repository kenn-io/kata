package cron

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/jobform"
	"go.kenn.io/kit/tui/termtext"
)

func safe(s string) string { return termtext.SanitizeLine(s) }
func (m *jobForm) bannerText(text string, style lipgloss.Style) bodyLines {
	var lines bodyLines
	for _, line := range wrapText(text, max(1, m.width)) {
		lines = append(lines, bodyLine{Text: style.Render(line)})
	}
	return lines
}
func (m *jobForm) bannerLines() bodyLines {
	if m.discard {
		return bodyLines{{Text: attentionStyle.Render("discard changes? y/n")}}
	}
	switch m.phase {
	case phaseConflict:
		if m.newer == nil {
			out := m.bannerText("conflict · newer definition unavailable", attentionStyle)
			out = append(out, m.bannerText("original definition guard kept", subtleStyle)...)
			return append(out, m.bannerText("esc asks to discard this draft · reopen e to start from Kata's version", subtleStyle)...)
		}
		out := bodyLines{{Text: attentionStyle.Render("Kata has a newer definition of " + safe(m.job.Name))}}
		attempted, err := m.definition()
		if m.attempt != nil {
			attempted = m.attempt.job.Definition
		}
		if err != nil {
			out = append(out, bodyLine{Text: failStyle.Render("cannot decode attempted definition: " + safe(err.Error()))})
		} else {
			a, aerr := defs.DecodeJob(attempted)
			b, berr := defs.DecodeJob(m.newer.Definition)
			if aerr != nil || berr != nil {
				out = append(out, bodyLine{Text: failStyle.Render("cannot decode retained or newer definition")})
			} else {
				for _, change := range jobform.Diff(a, b, m.Value("name"), m.newer.Name, m.formContext()) {
					for _, line := range wrapText(fmt.Sprintf("%s   yours: %s   Kata: %s", change.Field, change.Yours, change.Kata), max(1, m.width-2)) {
						out = append(out, bodyLine{Text: "  " + detailMetaStyle.Render(line)})
					}
				}
			}
		}
		out = append(out, bodyLine{Text: subtleStyle.Render("original definition guard kept")})
		out = append(out, m.bannerText("esc asks to discard this draft · reopen e to start from Kata's version", subtleStyle)...)
		return out
	case phaseSaving:
		return bodyLines{{Text: subtleStyle.Render("saving…")}}
	case phaseUncertain:
		out := m.bannerText("Readback unavailable · "+safe(m.attempt.job.Name), attentionStyle)
		out = append(out, m.bannerText("ctrl+s reads back and retries the same draft · esc asks to discard this draft", attentionStyle)...)
		return append(out, m.bannerText("outcome uncertain: "+safe(m.status), attentionStyle)...)
	}
	if m.status != "" {
		return m.bannerText(safe(m.status), failStyle)
	}
	return nil
}
func (m *jobForm) recurringDropNotice() bool {
	if m.Value("mode") != "recurring" || get(m.raw, "action.kind") == "notify" && get(m.raw, "issue.kind") == "per-run" {
		return false
	}
	for _, key := range []string{"description", "tags", "profile", "checkout", "timeout", "secret_refs", "input"} {
		if m.Value(key) != "" {
			return true
		}
	}
	return false
}
func (m *jobForm) renderLines() (lines bodyLines, focus int) {
	fields := m.fields()
	m.index = max(0, min(m.index, len(fields)-1))
	title := "new job"
	if !m.creating {
		title = "edit " + safe(m.job.Name)
	}
	heading := titleStyle.Render(title)
	if m.editorContext.Project != "" {
		heading += subtleStyle.Render(" · project " + safe(m.editorContext.Project))
	}
	lines = append(lines, bodyLine{Text: heading}, bodyLine{})
	banner := m.bannerLines()
	bannerStart := len(lines)
	lines = append(lines, banner...)
	if len(banner) > 0 {
		lines = append(lines, bodyLine{})
	}
	frozen := m.locked()
	for i, f := range fields {
		row := len(lines)
		selected := i == m.index
		prefix := "  "
		if selected {
			prefix = cursorStyle.Render("▶ ")
			focus = row
		}
		label := prefix + subtleStyle.Render(pad(f.Label, 18)) + " "
		display, _ := jobform.Get(f, m.draft(), m.formContext())
		plain := safe(display)
		value := plain
		if f.Kind == jobform.TextArea {
			hard := strings.Split(display, "\n")
			suffix := ""
			if len(hard) > 1 {
				suffix = fmt.Sprintf(" … (+%d lines)", len(hard)-1)
			}
			value = truncate(hard[0], max(1, min(70, m.width-21)-ansi.StringWidth(suffix))) + subtleStyle.Render(suffix)
		}
		if plain == "—" {
			value = subtleStyle.Render("—")
		}
		if f.Kind == jobform.ReadOnly || frozen {
			value = neutralStyle.Render(ansi.Strip(value))
		}
		active := m.active == f.Key
		if active && f.Kind == jobform.TextArea {
			value = ""
		} else if active {
			runes := []rune(m.buffer)
			caret := max(0, min(m.caret, len(runes)))
			before := safe(string(runes[:caret]))
			text := before + "▏" + safe(string(runes[caret:]))
			available := max(1, m.width-21)
			offset := max(0, ansi.StringWidth(before)+1-available)
			value = ansi.TruncateLeft(text, offset, "")
		}
		lines = append(lines, bodyLine{Text: label + value})
		if message := m.fieldErrors[f.Key]; message != "" {
			if m.errorFocus == f.Key {
				focus = len(lines)
			}
			lines = append(lines, bodyLine{Text: "  " + failStyle.Render(safe(message))})
		}
		if active && f.Kind == jobform.TextArea {
			cols := m.textareaCols()
			layout := layoutText(m.buffer, cols)
			text, caretRow := layout.withCaret(m.caret, cols)
			rows := max(1, min(10, m.height-8))
			offset := max(0, caretRow-rows+1)
			offset = min(offset, max(0, len(text)-rows))
			for n := offset; n < min(len(text), offset+rows); n++ {
				if n == caretRow && m.errorFocus != f.Key {
					focus = len(lines)
				}
				lines = append(lines, bodyLine{Text: "  " + text[n]})
			}
			lines = append(lines, bodyLine{Text: "      " + subtleStyle.Render(formEditingHelp().Render(max(1, m.width-6)))})
		} else if selected && f.Help != "" {
			for help := range strings.SplitSeq(ansi.Wrap(m.fieldHelp(f), max(1, m.width-21), ""), "\n") {
				lines = append(lines, bodyLine{Text: strings.Repeat(" ", 21) + help})
			}
		}
		if f.Key == "mode" && m.recurringDropNotice() {
			for _, notice := range wrapText("Options are preserved. Recurring jobs require empty options; remove them with kata cron job update before converting.", max(1, m.width-2)) {
				lines = append(lines, bodyLine{Text: "  " + attentionStyle.Render(notice)})
			}
		}
	}
	help := footerHelp{Full: []helpItem{hintGroup("j/k", "move", 0, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown), hintGroup("l/\u2192 or enter", "edit", 0, keys.Open, keys.Right, keys.Accept), hintGroup("space", "toggle", 0, keys.Toggle), hintGroup("ctrl+s", "save job", 0, keys.Save, keys.SaveAlt), hintGroup("esc", "abandon edit", 0, keys.Cancel)}, Short: []helpItem{hintGroup("j/k", "move", 0, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown), hintGroup("enter", "edit", 0, keys.Accept), hintGroup("ctrl+s", "save job", 0, keys.Save, keys.SaveAlt), hintGroup("esc", "abandon", 0, keys.Cancel)}}
	if m.phase == phaseSaving {
		items := []helpItem{hintGroup("ctrl+c", "quit", 0, keys.Interrupt)}
		help = footerHelp{Full: items, Short: items}
	} else if m.locked() {
		items := []helpItem{hintGroup("j/k", "move", 0, keys.Down, keys.Up)}
		if m.canRetry() {
			items = append(items, hintGroup("ctrl+s", "read back/retry", 0, keys.Save))
		}
		items = append(items, hintGroup("esc", "discard draft?", 0, keys.Cancel))
		help = footerHelp{Full: items, Short: items}
	}
	lines = append(lines, bodyLine{}, bodyLine{Text: help.Render(m.width)})
	if len(banner) > 0 && (m.bannerFocus || m.discard) {
		focus = bannerStart
	}
	for i := range lines {
		lines[i].Text = ansi.Truncate(lines[i].Text, max(1, m.width), "")
	}
	return lines, focus
}
func (m *jobForm) View() string {
	lines, focus := m.renderLines()
	visible, _ := window(lines, max(1, m.height), &m.scroll, true, focusLine(focus))
	text := make([]string, len(visible))
	for i, line := range visible {
		text[i] = ansi.Truncate(line.Text, max(1, m.width), "")
	}
	return strings.Join(text, "\n")
}

func (m *jobForm) fieldHelp(f jobform.Field) string {
	if f.Kind == jobform.ReadOnly && f.ReadOnlyReason != "" {
		return subtleStyle.Render(safe(f.ReadOnlyReason))
	}
	return subtleStyle.Render(safe(f.Help))
}
