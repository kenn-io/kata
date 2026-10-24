package cron

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func renderTabs(labels []string, active, tableWidth, width int, info ...tabInfoData) (string, []tabHit) {
	if len(labels) == 0 {
		return "", nil
	}
	active = max(0, min(active, len(labels)-1))
	start, end := active, active+1
	size := func(a, b int) int {
		n := 1
		for _, label := range labels[a:b] {
			n += ansi.StringWidth(plain(label)) + 3
		}
		return n
	}
	tabLimit := width
	status := tabInfoData{}
	if len(info) > 0 {
		status = info[0]
	}
	if text := status.minimum(); text != "" {
		minimum := size(active, active+1)
		reserved := min(ansi.StringWidth(text), max(0, width-minimum-1))
		tabLimit = max(minimum, width-reserved-1)
	}
	for {
		changed := false
		if start > 0 && size(start-1, end) <= tabLimit {
			start--
			changed = true
		}
		if end < len(labels) && size(start, end+1) <= tabLimit {
			end++
			changed = true
		}
		if !changed {
			break
		}
	}
	var top, mid, bot strings.Builder
	hits := []tabHit{}
	x := 1
	for i := start; i < end; i++ {
		label := plain(labels[i])
		w := ansi.StringWidth(label) + 2
		rule := strings.Repeat("─", w)
		if i == start {
			top.WriteString("╭")
			mid.WriteString(subtleStyle.Render("│"))
			edge := "┴"
			if i == active {
				edge = "┘"
			}
			bot.WriteString(edge)
		}
		top.WriteString(rule)
		style := tabInactive
		if i == active {
			style = tabActive
		}
		mid.WriteString(style.Render(" " + label + " "))
		hits = append(hits, tabHit{Index: i, X0: x, X1: x + w})
		x += w + 1
		if i == active {
			bot.WriteString(strings.Repeat(" ", w))
		} else {
			bot.WriteString(rule)
		}
		if i == end-1 {
			top.WriteString("╮")
			mid.WriteString(subtleStyle.Render("│"))
			edge := "┴"
			if i == active {
				edge = "└"
			}
			bot.WriteString(edge)
		} else {
			top.WriteString("┬")
			mid.WriteString(subtleStyle.Render("│"))
			edge := "┴"
			if i == active {
				edge = "└"
			} else if i+1 == active {
				edge = "┘"
			}
			bot.WriteString(edge)
		}
	}
	if text := tabStatus(status, max(0, width-ansi.StringWidth(mid.String())-1)); text != "" {
		mid.WriteString(strings.Repeat(" ", max(1, width-ansi.StringWidth(mid.String())-ansi.StringWidth(text))))
		mid.WriteString(text)
	}
	bottom := bot.String() + strings.Repeat("─", max(0, tableWidth-ansi.StringWidth(bot.String())))
	return ansi.Truncate(subtleStyle.Render(top.String()), width, "") + "\n" + ansi.Truncate(mid.String(), width, "") + "\n" + ansi.Truncate(subtleStyle.Render(bottom), width, ""), hits
}

type tabCount struct {
	Project       string
	Running, Held int
}

type tabInfoData struct {
	Hint    string
	Counts  []tabCount
	Problem string
}

// tabStatus prefers health, then totals, then the per-project breakdown.
func tabStatus(info tabInfoData, width int) string {
	if width <= 0 {
		return ""
	}
	withHint := func(text string) string {
		if info.Hint == "" {
			return text
		}
		if text == "" {
			return subtleStyle.Render(truncate(info.Hint, width))
		}
		combined := subtleStyle.Render(info.Hint+" · ") + text
		if ansi.StringWidth(combined) <= width {
			return combined
		}
		return text
	}
	problem := ""
	if info.Problem != "" {
		problem = attentionStyle.Render(plain(info.Problem))
	}
	join := func(parts []string) string {
		if problem != "" {
			parts = append(parts, problem)
		}
		return strings.Join(parts, subtleStyle.Render(" · "))
	}

	for stage := range 3 {
		parts := []string{}
		for _, c := range info.Counts {
			if c.Running == 0 && c.Held == 0 {
				continue
			}
			label := plain(c.Project)
			if stage > 0 {
				label = truncate(label, 12)
			}
			if label != "" {
				label = subtleStyle.Render(label + ": ")
			}
			parts = append(parts, label+countSummary(c, stage == 2))
		}
		if s := join(parts); ansi.StringWidth(s) <= width {
			return withHint(s)
		}
	}
	total := tabCount{}
	for _, c := range info.Counts {
		total.Running += c.Running
		total.Held += c.Held
	}
	for _, c := range []tabCount{total, {Held: total.Held}, {Running: total.Running}} {
		s := countSummary(c, true)
		parts := []string{}
		if s != "" {
			parts = append(parts, s)
		}
		if s = join(parts); ansi.StringWidth(s) <= width {
			return withHint(s)
		}
	}
	return ansi.Truncate(problem, width, "")
}

func countSummary(c tabCount, compact bool) string {
	parts := []string{}
	if c.Running > 0 {
		text := fmt.Sprintf("%d running", c.Running)
		if compact {
			text = fmt.Sprintf("%dr", c.Running)
		}
		parts = append(parts, subtleStyle.Render(text))
	}
	if c.Held > 0 {
		text := fmt.Sprintf("%d parked", c.Held)
		if compact {
			text = fmt.Sprintf("%dp", c.Held)
		}
		parts = append(parts, attentionStyle.Render(text))
	}
	gap := " · "
	if compact {
		gap = "/"
	}
	return strings.Join(parts, subtleStyle.Render(gap))
}

func (info tabInfoData) minimum() string {
	if info.Problem != "" {
		return plain(info.Problem)
	}
	total := tabCount{}
	for _, c := range info.Counts {
		total.Running += c.Running
		total.Held += c.Held
	}
	return countSummary(total, true)
}

var folderTabLabels = []string{"kata カタ", "jobs", "runs", "workflows"}

// Tabs renders the source's three-row native navigation bar.
func Tabs(active, width int) string {
	text, _ := renderTabs(folderTabLabels, active, width, width)
	return text
}

// TabAt hit-tests the visible tab labels on row one.
func TabAt(active, width, x, y int) (int, bool) {
	_, tabs := renderTabs(folderTabLabels, active, width, width)
	return (hits{Width: width, TabY: 1, Tabs: tabs}).TabAt(x, y)
}

// Tabs renders navigation with the loaded native observations and catalog problems.
func (m *model) Tabs(active, width int) string {
	text, _ := renderTabs(folderTabLabels, active, width, width, m.tabInfo())
	return text
}

// TabAt uses the same status reservation as Tabs for narrow-terminal hit testing.
func (m *model) TabAt(active, width, x, y int) (int, bool) {
	_, tabs := renderTabs(folderTabLabels, active, width, width, m.tabInfo())
	return (hits{Width: width, TabY: 1, Tabs: tabs}).TabAt(x, y)
}

func (m *model) tabInfo() tabInfoData {
	counts := map[string]int{}
	for _, r := range m.data.Runs {
		if r.Status != "running" {
			continue
		}
		project := ""
		if m.showProjects() {
			project = m.projectName(r.ProjectKey)
		}
		counts[project]++
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	info := tabInfoData{}
	for _, name := range names {
		info.Counts = append(info.Counts, tabCount{Project: name, Running: counts[name]})
	}
	if len(m.data.Problems) > 0 {
		info.Problem = strings.Join(m.data.Problems, " · ")
	}
	return info
}

// TabsVisible follows the source: lists and the project picker have tabs;
// detail and editor screens use the full terminal.
func (m *model) TabsVisible() bool {
	return m.editor == nil && (m.currentView() == viewList || len(m.projectChoices) > 0)
}
