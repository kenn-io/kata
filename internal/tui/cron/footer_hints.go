package cron

import (
	"strings"
	"time"

	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/helprender"

	"github.com/charmbracelet/x/ansi"
)

type helpItem struct {
	Key, Desc string
	Prio      int
	bindings  []string
}
type footerHelp struct{ Full, Short []helpItem }

func helpText(items []helpItem) string {
	parts := []string{}
	for _, item := range items {
		parts = append(parts, helpKeyStyle.Render(plain(item.Key))+" "+helpDescStyle.Render(plain(item.Desc)))
	}
	return strings.Join(parts, helpDescStyle.Render(" · "))
}
func (h footerHelp) Render(width int) string {
	if width <= 0 {
		return ""
	}
	full := helpText(h.Full)
	if ansi.StringWidth(full) <= width && full != "" {
		return full
	}
	items := append([]helpItem(nil), h.Short...)
	if len(items) == 0 {
		items = append(items, h.Full...)
	}
	for {
		text := helpText(items)
		if ansi.StringWidth(text) <= width {
			return text
		}
		remove := -1
		for i, item := range items {
			if item.Key == "q" && item.Desc == "quit" || item.Key == "M" && item.Desc == "mouse" {
				continue
			}
			if remove < 0 || item.Prio <= items[remove].Prio {
				remove = i
			}
		}
		if remove < 0 {
			return ansi.Truncate(text, width, "…")
		}
		items = append(items[:remove], items[remove+1:]...)
	}
}

type statusKind int

const (
	statusInfo statusKind = iota
	statusSuccess
	statusError
)

type statusNotice struct {
	Text  string
	Kind  statusKind
	Until time.Time
}
type searchState struct {
	Query   string
	Editing bool
}

func errorText(s string) string { return "error: " + strings.TrimPrefix(plain(s), "error: ") }
func footer(status statusNotice, conditions []string, search searchState) bodyLines {
	var b bodyLines
	for _, condition := range conditions {
		if condition != "" {
			if len(b) == 0 {
				b = append(b, bodyLine{})
			}
			b = append(b, bodyLine{Text: errorStyle.Render(errorText(condition))})
		}
	}
	if status.Text != "" {
		if len(b) == 0 {
			b = append(b, bodyLine{})
		}
		text := plain(status.Text)
		role := statusStyle
		switch status.Kind {
		case statusError:
			text = errorText(text)
			role = errorStyle
		case statusSuccess:
			role = toastStyle
		}
		b = append(b, bodyLine{Text: role.Render(text)})
	}
	if search.Editing || search.Query != "" {
		b = append(b, bodyLine{})
		text := subtleStyle.Render("filter: " + plain(search.Query) + "  (esc clears)")
		if search.Editing {
			text = chipActive.Render("/"+plain(search.Query)+"▏") + subtleStyle.Render("  enter keep · esc clear")
		}
		b = append(b, bodyLine{Text: text})
	}
	return b
}

func jobsHelp() footerHelp {
	full := []helpItem{hintGroup("tab", "tabs", 6, keys.NextTab, keys.PrevTab), keys.Search.hint(14), hintGroup("[ ]", "page", 7, keys.PrevPage, keys.NextPage, keys.PrevPageAlt, keys.NextPageAlt), hintGroup("j/k", "move", 16, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown), hintGroup("l/\u2192", "open", 15, keys.Open, keys.Right, keys.Accept), keys.Pause.hint(11), keys.New.hint(13), keys.Edit.hint(5), keys.Delete.hint(4), keys.Issue.hint(2), keys.Kata.hint(1), keys.Reload.hint(1), keys.Layout.hint(1), hintGroup("q", "quit", 99, keys.Quit, keys.Interrupt)}
	return footerHelp{Full: append(full, navigationHints()...)}
}

func runsHelp() footerHelp {
	full := []helpItem{hintGroup("tab", "tabs", 0, keys.NextTab, keys.PrevTab), keys.Search.hint(0), hintGroup("[ ]", "page", 0, keys.PrevPage, keys.NextPage, keys.PrevPageAlt, keys.NextPageAlt), hintGroup("j/k", "move", 0, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown), hintGroup("l/\u2192", "open", 0, keys.Open, keys.Right, keys.Accept), keys.Issue.hint(0), keys.Kata.hint(1), keys.Reload.hint(1), keys.Layout.hint(1), hintGroup("q", "quit", 99, keys.Quit, keys.Interrupt)}
	short := []helpItem{hintGroup("tab", "tabs", 5, keys.NextTab, keys.PrevTab), keys.Search.hint(7), hintGroup("[ ]", "page", 6, keys.PrevPage, keys.NextPage, keys.PrevPageAlt, keys.NextPageAlt), hintGroup("j/k", "move", 8, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown), hintGroup("l/\u2192", "open", 9, keys.Open, keys.Right, keys.Accept), keys.Layout.hint(1), hintGroup("q", "quit", 99, keys.Quit, keys.Interrupt)}
	return footerHelp{Full: append(full, navigationHints()...), Short: short}
}

func workflowHelp() footerHelp {
	full := []helpItem{hintGroup("tab", "tabs", 40, keys.NextTab, keys.PrevTab), keys.Search.hint(70), hintGroup("[ ]", "page", 50, keys.PrevPage, keys.NextPage, keys.PrevPageAlt, keys.NextPageAlt), hintGroup("j/k", "move", 90, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown), hintGroup("space", "steps", 10, keys.Toggle), hintGroup("l/\u2192", "open", 95, keys.Open, keys.Right, keys.Accept), keys.Edit.hint(60), keys.Reload.hint(20), keys.Kata.hint(1), keys.Layout.hint(1), hintGroup("q", "quit", 1000, keys.Quit, keys.Interrupt)}
	// Short drops steps, edit and reload.
	short := append(append(append([]helpItem(nil), full[:4]...), full[5]), full[8:]...)
	return footerHelp{Full: append(full, navigationHints()...), Short: short}
}

func (m *model) jobDetailHelp() footerHelp {
	items := []helpItem{hintGroup("h/\u2190", "back", 10, keys.Back, keys.Left, keys.Cancel, keys.Backspace), keys.Kata.hint(1), keys.Reload.hint(1), hintGroup("ctrl+d/u", "scroll", 1, keys.HalfDown, keys.HalfUp, keys.PageUp, keys.PageDown)}
	v := m.selectedJobView()
	if v == nil {
		return footerHelp{Full: append(items, hintGroup("q", "quit", 99, keys.Quit, keys.Interrupt))}
	}
	if len(v.runIndexes) > 0 {
		items = append(items, hintGroup("j/k", "move", 8, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown), hintGroup("l/\u2192", "open run", 9, keys.Open, keys.Right, keys.Accept))

		items = append(items, keys.Issue.hint(4))
	}
	items = append(items, keys.Pause.hint(2), keys.Edit.hint(3))
	items = append(items, keys.Delete.hint(1), hintGroup("q", "quit", 99, keys.Quit, keys.Interrupt))
	return footerHelp{Full: items, Short: items}
}

func (m *model) runDetailHelp() footerHelp {
	items := []helpItem{hintGroup("h/\u2190", "back", 8, keys.Back, keys.Left, keys.Cancel, keys.Backspace), hintGroup("l/\u2192", "open job", 7, keys.Open, keys.Right, keys.Accept), hintGroup("j/k", "scroll", 1, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown)}
	if r, ok := m.detailRun(); ok {
		if value(r.IssueUID) != "" {
			items = append(items, keys.Issue.hint(3))
		}
	}
	items = append(items, keys.Kata.hint(1), hintGroup("r", "refresh", 2, keys.Reload), hintGroup("ctrl+d/u", "scroll", 1, keys.HalfDown, keys.HalfUp, keys.PageUp, keys.PageDown), hintGroup("q", "quit", 99, keys.Quit, keys.Interrupt))
	return footerHelp{Full: items, Short: items}
}

func workflowDetailHelp() footerHelp {
	full := []helpItem{hintGroup("h/\u2190", "back", 100, keys.Back, keys.Left, keys.Cancel, keys.Backspace), hintGroup("j/k", "move", 90, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown), hintGroup("space", "step", 50, keys.Toggle), hintGroup("l/\u2192", "open", 95, keys.Open, keys.Right, keys.Accept), keys.Edit.hint(60), keys.Reload.hint(20), keys.Kata.hint(1), hintGroup("ctrl+d/u", "scroll", 1, keys.HalfDown, keys.HalfUp, keys.PageUp, keys.PageDown), hintGroup("q", "quit", 1000, keys.Quit, keys.Interrupt)}
	return footerHelp{Full: full, Short: []helpItem{full[0], full[1], full[3], full[4], full[6], full[7]}}
}

func hintGroup(display, description string, priority int, bindings ...key) helpItem {
	item := helpItem{Key: display, Desc: description, Prio: priority}
	for _, binding := range bindings {
		item.bindings = append(item.bindings, binding.Keys...)
	}
	return item
}

func navigationHints() []helpItem {
	return []helpItem{
		hintGroup("1–4", "tabs", 0, keys.KataTab, keys.JobsTab, keys.RunsTab, keys.WorkflowsTab),
		hintGroup("ctrl+d/u", "scroll", 0, keys.HalfDown, keys.HalfUp, keys.PageDown, keys.PageUp),
	}
}

func formEditingHelp() footerHelp {
	return footerHelp{Full: []helpItem{
		hintGroup("←/→/↑/↓", "caret", 0, keys.Left, keys.Right, keys.ScrollUp, keys.ScrollDown),
		hintGroup("home/end", "line", 0, keys.Home, keys.End, keys.LineHome, keys.LineEnd),
		hintGroup("ctrl+home/end", "text", 0, keys.DocHome, keys.DocEnd),
		hintGroup("backspace/del", "erase", 0, keys.Backspace, keys.DeleteChar),
		hintGroup("ctrl+s/S", "save", 0, keys.Save, keys.SaveAlt),
		hintGroup("y/n", "discard/keep", 0, keys.Yes, keys.New),
	}}
}

// Lines renders every footer action using Kata's reflowed help table.
func (h footerHelp) Lines(width int) []string {
	if width <= 0 {
		return nil
	}
	inner := max(width-2, 1)
	items := make([]helplayout.HelpItem, 0, len(h.Full))
	for _, item := range h.Full {
		items = append(items, helplayout.HelpItem{Key: plain(item.Key), Description: plain(item.Desc)})
	}
	rows := helplayout.ReflowRows([][]helplayout.HelpItem{items}, inner, helprender.ColumnGap)
	body := helprender.RenderHelpTable(rows, helprender.Styles{Key: helpKeyStyle, Description: helpDescStyle, BorderColor: panelInactiveBorder})
	if body == "" {
		return []string{footerBarStyle.Render(pad("", inner))}
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		line = ansi.Truncate(line, inner, "…")
		line += strings.Repeat(" ", max(0, inner-ansi.StringWidth(line)))
		lines[i] = ansi.Truncate(footerBarStyle.Render(line), width, "")
	}
	return lines
}
