package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// federationNoticeLine is the one-line confirmation of the last name change,
// or an empty line when there is none.
func federationNoticeLine(m Model) string {
	if m.federation.notice == "" {
		return ""
	}
	return subtleStyle.Render(truncate(sanitizeForLine(m.federation.notice), m.width))
}

// federationFooterLines draws the hint row. Over a modal it is the help
// table; otherwise the hints wrap between items so none is clipped at the
// terminal's edge.
func federationFooterLines(m Model, hints ...string) []string {
	if rows := modalFirstHelpRows(m.modal, nil); rows != nil {
		return strings.Split(renderFooterHelpTable(rows, m.width), "\n")
	}
	var lines []string
	line := ""
	for _, hint := range hints {
		switch {
		case line == "":
			line = hint
		case m.width > 0 && runewidth.StringWidth(line)+2+runewidth.StringWidth(hint) > m.width:
			lines = append(lines, line)
			line = hint
		default:
			line += "  " + hint
		}
	}
	lines = append(lines, line)
	for i, l := range lines {
		lines[i] = subtleStyle.Render(l)
	}
	return lines
}

func renderFederationFooter(m Model, hints ...string) string {
	return strings.Join(federationFooterLines(m, hints...), "\n")
}

// federationErrorLines wraps a daemon error, keeping its own line breaks, so
// nothing is clipped at the terminal's edge.
func federationErrorLines(err error, width int) []string {
	if err == nil {
		return nil
	}
	if width <= 0 {
		width = 80
	}
	var lines []string
	for raw := range strings.SplitSeq(err.Error(), "\n") {
		for _, wrapped := range wrapBody(sanitizeForLine(raw), width) {
			lines = append(lines, errorStyle.Render(wrapped))
		}
	}
	return lines
}

// federationPromptLine is one row of the name prompt. Optional rows are
// dropped, lowest priority number kept longest, when the terminal is short;
// required rows (priority 0) always stay.
type federationPromptLine struct {
	text     string
	priority int
}

// renderFederationNamePrompt draws the one-field name prompt. The prompt row,
// the daemon's error and the action hints are required; the title, the
// context and the rules shrink first.
func renderFederationNamePrompt(m Model) string {
	d := m.federation.nameDraft
	width := m.width
	if width <= 0 {
		width = 80
	}
	const rules = "Lowercase letters, digits and hyphens, starting with a letter, up to 32."
	title, emptyNote := "Name this spoke on its hub", "An empty name clears the spoke's name."
	context := "hub: " + federationHubDisplay(d.HubURL) + " · project: " + d.ProjectName
	action := "[enter] save"
	if d.forEnrollment() {
		title, emptyNote = "Name this spoke for the enrollment", "An empty name sends no name with the enrollment."
		context = "hub: " + d.HubURL + " · project: " + d.ProjectName
		action = "[enter] keep"
	}
	footer := federationFooterLines(m, action, "[esc] cancel")
	if m.federation.op.running {
		footer = federationFooterLines(m, "saving the name on the hub...")
	}
	errLines := federationErrorLines(m.federation.op.err, width)
	prompt := federationNameInputLine(d.Input, width)
	// An error that cannot fit even with every optional row gone keeps its
	// first lines and ends in an ellipsis.
	if m.height > 0 {
		if room := m.height - 1 - len(footer); len(errLines) > room {
			errLines = errLines[:max(room, 1)]
			errLines[len(errLines)-1] = errorStyle.Render("…")
		}
	}
	lines := []federationPromptLine{
		{titleStyle.Render(truncate(title, width)), 4},
		{subtleStyle.Render(truncate(sanitizeForLine(context), width)), 3},
		{"", 6},
		{subtleStyle.Render(truncate(rules, width)), 2},
		{subtleStyle.Render(truncate(emptyNote, width)), 1},
		{"", 5},
		{prompt, 0},
	}
	if len(errLines) > 0 {
		lines = append(lines, federationPromptLine{"", 7})
		for _, l := range errLines {
			lines = append(lines, federationPromptLine{l, 0})
		}
	}
	lines = append(lines, federationPromptLine{"", 8})
	for _, l := range footer {
		lines = append(lines, federationPromptLine{l, 0})
	}
	return strings.Join(fitFederationPromptLines(lines, m.height), "\n")
}

// federationNameInputLine shows the tail of the typed text, so the newest
// characters stay visible however long the input grows.
func federationNameInputLine(input string, width int) string {
	const label = "name: "
	shown := sanitizeForLine(input)
	room := width - len(label) - 1
	if over := ansi.StringWidth(shown) - room; over > 0 && room > 1 {
		shown = ansi.TruncateLeft(shown, over+1, "…")
	}
	return label + shown + "▏"
}

func fitFederationPromptLines(lines []federationPromptLine, height int) []string {
	keep := make([]bool, len(lines))
	spare := len(lines)
	if height > 0 {
		spare = height
	}
	for i, l := range lines {
		if l.priority == 0 {
			spare--
			keep[i] = true
		}
	}
	optional := []int{}
	for i, l := range lines {
		if l.priority > 0 {
			optional = append(optional, i)
		}
	}
	sort.SliceStable(optional, func(a, b int) bool {
		return lines[optional[a]].priority < lines[optional[b]].priority
	})
	for _, i := range optional {
		if spare > 0 {
			keep[i] = true
			spare--
		}
	}
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		if keep[i] {
			out = append(out, l.text)
		}
	}
	return out
}
