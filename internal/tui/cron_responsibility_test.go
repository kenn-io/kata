package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// A creator and a previous executor must not become future execution owners.
func TestCronResponsibilityListsAndDetails(t *testing.T) {
	for _, tab := range []rune{'2', '3', '4'} {
		m := sendRune(cronFixture(t), tab)
		m.width, m.height = 120, 70
		m.cron.data.jobs[0].Author = "creator"
		m.cron.data.workflows[0].Author = "flow-creator"
		m.cron.data.runs[0].Actor = "runner"
		m.cron.data.runs[0].Teammate = new("helper")
		m.cron.data.jobs[0].Definition.Options = map[string]any{"adapter": "adapter-a", "executor": "preferred-worker"}
		title, header, lines := cronScreen(m)
		require.Contains(t, header, "Actor")
		require.Contains(t, header, "Runtime")
		require.Len(t, lines, 1)
		require.Contains(t, title, "1 ")
		if tab == '3' {
			require.Contains(t, lines[0].text, "runner")
			require.Contains(t, lines[0].text, "executor-a")
		} else {
			require.Contains(t, header, "Author")
			require.Contains(t, lines[0].text, "creator")
			require.Equal(t, 2, strings.Count(lines[0].text, "unknown"))
			require.NotContains(t, lines[0].text, "executor-a")
		}
		m = sendKey(m, tea.KeyEnter)
		view := ansi.Strip(m.viewBody())
		require.Contains(t, view, "Actor:")
		require.Contains(t, view, "Runtime:")
		if tab == '2' {
			require.Contains(t, view, "adapter-a")
			require.Contains(t, view, "preferred-worker")
		}
		if tab != '3' {
			require.Contains(t, view, "unknown")
			require.Contains(t, view, "runner")
			require.Contains(t, view, "executor-a")
		} else {
			require.Contains(t, view, "helper")
		}
	}
}

func TestCronRuntimeUnreportedAndNative(t *testing.T) {
	m := cronFixture(t)
	m.cron.data.jobs[0].Definition.Action.Kind = "notify"
	m.cron.data.jobs[0].Definition.Issue.ParentUID = new("01ARZ3NDEKTSV4RRFFQ69G5FB4")
	_, _, lines := cronScreen(m)
	require.Contains(t, lines[0].text, "owning daemon")
	for _, label := range []*string{nil, new(" \t")} {
		m = sendRune(m, '3')
		m.cron.data.runs[0].ExecutorLabel = label
		_, _, lines = cronScreen(m)
		require.Contains(t, lines[0].text, "unreported")
		view := ansi.Strip(sendKey(m, tea.KeyEnter).viewBody())
		require.Contains(t, view, "unreported")
	}
}

func TestCronAttributionFiltersFullLabels(t *testing.T) {
	for _, tab := range []rune{'2', '3', '4'} {
		m := sendRune(cronFixture(t), tab)
		label := "distinctive-actor-with-long-name"
		m.cron.data.jobs[0].Author = label
		m.cron.data.workflows[0].Author = label
		m.cron.data.runs[0].Actor = label
		m = m.cronFilter(label)
		_, _, lines := cronScreen(m)
		require.Len(t, lines, 1, "tab %c", tab)
		m = m.cronFilter("missing-actor")
		_, _, lines = cronScreen(m)
		require.Empty(t, lines)
	}
	m := cronFixture(t)
	m.cron.data.jobs[0].Definition.Options = map[string]any{"adapter": "adapter-search-value"}
	m = m.cronFilter("adapter-search-value")
	_, _, lines := cronScreen(m)
	require.Len(t, lines, 1)
}

func TestCronAttributionRowsFitAndSanitize(t *testing.T) {
	for _, width := range []int{40, 60, 80, 120} {
		for _, tab := range []rune{'2', '3', '4'} {
			m := sendRune(cronFixture(t), tab)
			m.width = width
			m.cron.data.jobs[0].Author = "creator\n\x1b[31mspoof"
			m.cron.data.workflows[0].Author = "creator\n\x1b[31mspoof"
			m.cron.data.runs[0].Actor = "runner\n\x1b[31mspoof"
			m.cron.data.runs[0].ExecutorLabel = new("runtime\n\x1b[31mspoof")
			_, header, lines := cronScreen(m)
			require.LessOrEqual(t, ansi.StringWidth(header)+2, width)
			require.Len(t, lines, 1)
			require.NotContains(t, lines[0].text, "\n")
			require.NotContains(t, lines[0].text, "\x1b")
			require.LessOrEqual(t, ansi.StringWidth(lines[0].text)+2, width)
		}
	}
}

// Resizing changes visible columns, never which records match a query.
func TestCronFilterIndependentOfWidth(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		width := hegel.Draw(ht, hegel.Integers(40, 140))
		for _, tc := range []struct {
			tab   rune
			query string
		}{
			{'2', "enabled"}, {'2', "0 7 * * *"}, {'2', "workflow Summary flow"},
			{'3', "Daily summary"}, {'3', "Report ready"},
			{'3', "05-01 10:00"}, {'3', "05-01 11:00"},
			{'4', "1 job"}, {'4', "05-01 12:00"},
		} {
			m := sendRune(cronFixture(t), tc.tab)
			m.width = width
			m = m.cronFilter(tc.query)
			_, _, lines := cronScreen(m)
			require.Len(ht, lines, 1, "tab %c query %s width %d", tc.tab, tc.query, width)
		}
	}, hegel.WithTestCases(1000))
}

func TestCronCompactRunsRetainJobName(t *testing.T) {
	m := sendRune(cronFixture(t), '3')
	m.width = 69
	_, header, lines := cronScreen(m)
	require.Contains(t, header, "Job")
	require.Contains(t, lines[0].text, "Daily summary")
}

func TestCronHiddenStateSearchAt40(t *testing.T) {
	m := cronFixture(t)
	m.width = 40
	m = m.cronFilter("enabled")
	_, _, lines := cronScreen(m)
	require.Len(t, lines, 1)
}

func TestCronDefinitionResponsibilityIndependentOfHistory(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		actor := hegel.Draw(ht, hegel.Text())
		label := hegel.Draw(ht, hegel.Text())
		for _, tab := range []rune{'2', '4'} {
			m := sendRune(cronFixture(t), tab)
			_, _, before := cronScreen(m)
			m.cron.data.runs[0].Actor = actor
			m.cron.data.runs[0].ExecutorLabel = &label
			_, _, after := cronScreen(m)
			require.Equal(ht, before, after)
			m.cron.data.runs = nil
			_, _, after = cronScreen(m)
			require.Equal(ht, before, after)
		}
	}, hegel.WithTestCases(1000))
}

func TestCronAttributionHostileTextProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := "\x1b]0;title\a\n\t" + hegel.Draw(ht, hegel.Text())
		// Exercise the supported table sizes, including both sides of compact mode.
		width := hegel.Draw(ht, hegel.Integers(40, 240))
		for _, tab := range []rune{'2', '3', '4'} {
			m := sendRune(cronFixture(t), tab)
			m.width = width
			m.cron.data.jobs[0].Name, m.cron.data.jobs[0].Author = text, text
			m.cron.data.workflows[0].Name, m.cron.data.workflows[0].Author = text, text
			m.cron.data.runs[0].Actor, m.cron.data.runs[0].ExecutorLabel = text, &text
			_, _, lines := cronScreen(m)
			require.Len(ht, lines, 1)
			require.NotContains(ht, lines[0].text, "\n")
			require.NotContains(ht, lines[0].text, "\x1b")
			require.LessOrEqual(ht, ansi.StringWidth(lines[0].text)+2, width)
		}
	}, hegel.WithTestCases(1000))
}
