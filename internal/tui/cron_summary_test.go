package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

func cronSummaryFixture(t *testing.T) Model {
	t.Helper()
	m := cronFixture(t)
	m.cron.data.runs[0].Status = "failed"
	m.cron.data.runs[0].Summary = generated.SummaryResponse{
		Version: 1, Reason: new("agent_quota"), Message: new("reason: failed\nBudget exhausted\x1b[31m"),
		Usage: &generated.SummaryUsageResponse{Model: "model-large", CacheReadTokens: new(int64(10)), CacheWriteTokens: new(int64(20)), CostMicros: new(int64(0)), Currency: new("USD")},
		Steps: []generated.SummaryStepResponse{
			{Key: "review", Status: "failed", Attempts: new(int64(2)), ExitCode: new(int32(0)), Reason: new("agent_quota"), StartedAt: new("2026-05-01T10:00:00Z"), EndedAt: new("2026-05-01T10:00:02Z"), Message: new("Quota exhausted\x1b]8;;https://daemon.example\a text\x1b]8;;\a")},
			{Key: "publish", Status: "skipped"},
		}, LogRef: new("urn:example:run-log"),
	}
	return m
}

func TestCronSummaryDetail(t *testing.T) {
	m := cronSummaryFixture(t)
	for _, width := range []int{20, 40, 80} {
		m.width = width
		_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
		var text strings.Builder
		for _, line := range lines {
			require.LessOrEqual(t, ansi.StringWidth(line.text), width-2)
			require.NotContains(t, line.text, "\x1b")
			if strings.Contains(line.text, "urn:") {
				require.Empty(t, line.target.uid)
			}
			text.WriteString(line.text)
			text.WriteByte('\n')
		}
		compact := strings.Join(strings.Fields(text.String()), " ")
		for _, want := range []string{"Reason:", "agent_quota", "Usage", "Model:", "model-large", "Cost:", "0.000000 USD", "Steps (2)", "review", "Attempts:", "Exit:", "Duration:", "2s", "publish", "skipped", "Log ref:"} {
			// Wrapping may split a long value at narrow widths; assert full values
			// at the ordinary detail width; all widths must fit and strip escapes.
			if width == 80 {
				require.Contains(t, compact, want)
			}
		}
	}
	m.cron.data.runs[0].Summary.Usage.CostMicros = nil
	m.cron.data.runs[0].Summary.Steps[0].ExitCode = nil
	_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
	for _, line := range lines {
		require.NotContains(t, line.text, "Cost:")
		require.NotContains(t, line.text, "Exit:")
	}
}

func TestCronSummaryReasonPreview(t *testing.T) {
	m := cronSummaryFixture(t)
	m.width = 180
	m = sendRune(m, '3')
	_, _, lines := cronScreen(m)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0].text, "agent_quota")
	require.NotContains(t, lines[0].text, "\x1b")
	m.cron.data.runs[0].Summary.Reason = nil
	m.cron.data.runs[0].Summary.Message = new("reason: timeout\nStopped")
	_, _, lines = cronScreen(m)
	require.Contains(t, lines[0].text, "timeout")
}

func TestSnapshot_CronSummary(t *testing.T) {
	defer snapshotInit(t)()
	for _, width := range []int{32, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := cronSummaryFixture(t)
			m.width = width
			m.height = 60
			m = sendRune(m, '3')
			m = sendKey(m, tea.KeyEnter)
			assertGolden(t, fmt.Sprintf("cron-summary-%d", width), m.viewBody())
		})
	}
}

func TestCronSummaryDetailWrapProperty(t *testing.T) {
	m := cronSummaryFixture(t)
	hegel.Test(t, func(ht *hegel.T) {
		m.width = hegel.Draw(ht, hegel.Integers[int](20, 120))
		message := hegel.Draw(ht, hegel.Text().MaxSize(200))
		m.cron.data.runs[0].Summary.Message = &message
		m.cron.data.runs[0].Summary.Steps[0].Message = &message
		_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
		for _, line := range lines {
			if strings.ContainsRune(line.text, '\x1b') {
				ht.Fatalf("escape sequence in %q", line.text)
			}
			if ansi.StringWidth(line.text) > m.width-2 {
				ht.Fatalf("width %d overflows in %q", m.width, line.text)
			}
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestCronSummaryTabsFitRenderedRows(t *testing.T) {
	defer snapshotInit(t)()
	m := cronSummaryFixture(t)
	m.width, m.height = 40, 80
	m.cron.data.runs[0].Summary.Steps[0].Message = new("TABSTART\t\tTABEND\nNEXTLINE")
	m = sendRune(m, '3')
	m = sendKey(m, tea.KeyEnter)
	found := false
	for _, line := range strings.Split(m.viewBody(), "\n") {
		if strings.Contains(line, "TABSTART") || strings.Contains(line, "NEXTLINE") {
			found = true
			require.LessOrEqual(t, ansi.StringWidth(line), m.width, line)
		}
	}
	require.True(t, found)
}
