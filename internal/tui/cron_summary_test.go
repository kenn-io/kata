package tui

import (
	"fmt"
	"math"
	"strconv"
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
		for _, line := range lines {
			require.LessOrEqual(t, ansi.StringWidth(line.text), width-2)
			require.NotContains(t, line.text, "\x1b")
			if strings.Contains(line.text, "urn:") {
				require.Empty(t, line.target.uid)
			}
		}
	}
	_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
	var text strings.Builder
	for _, line := range lines {
		text.WriteString(line.text + "\n")
	}
	compact := strings.Join(strings.Fields(text.String()), " ")
	for _, want := range []string{"Reason:", "agent_quota", "Usage", "Model:", "model-large", "Cost:", "0.00 USD", "Steps (2)", "review", "Attempts:", "Exit:", "Duration:", "2s", "publish", "skipped", "Log ref:"} {
		require.Contains(t, compact, want)
	}
	m.cron.data.runs[0].Summary.Usage.CostMicros = nil
	m.cron.data.runs[0].Summary.Steps[0].ExitCode = nil
	_, _, lines = cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
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
			m.cron.data.runs[0].OccurrenceKey = new("request:fixture-run")
			m.cron.data.runs[0].Summary.Trigger = new("request")
			m.cron.data.runs[0].Summary.Reason = new("overlap_skipped")
			m.cron.data.runs[0].Summary.Steps[1].StartedAt = new("2026-05-01T10:00:00Z")
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
		message := strings.ReplaceAll(hegel.Draw(ht, hegel.Text().MaxSize(200)), "\x00", "")
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
	for line := range strings.SplitSeq(m.viewBody(), "\n") {
		if strings.Contains(line, "TABSTART") || strings.Contains(line, "NEXTLINE") {
			found = true
			require.LessOrEqual(t, ansi.StringWidth(line), m.width, line)
		}
	}
	require.True(t, found)
}

func TestCronSummaryReasonCarriers(t *testing.T) {
	for _, tc := range []struct {
		name, reason, message, preview, detail string
	}{
		{"legacy only", "", "reason: timeout", "reason: timeout", ""},
		{"legacy multiline", "", "reason: timeout\nBudget exhausted", "reason: timeout\nBudget exhausted", "Budget exhausted"},
		{"structured only", "timeout", "", "timeout", ""},
		{"matching only", "timeout", "reason: timeout", "timeout", ""},
		{"matching multiline", "timeout", "reason: timeout\nBudget exhausted", "timeout · Budget exhausted", "Budget exhausted"},
		{"conflicting", "timeout", "reason: failed\nBudget exhausted", "timeout · reason: failed\nBudget exhausted", "reason: failed\nBudget exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cronSummaryFixture(t)
			m.width = 100
			s := generated.SummaryResponse{Version: 1, Message: optionalSummaryTime(tc.message)}
			if tc.reason != "" {
				s.Reason = &tc.reason
			}
			require.Equal(t, tc.preview, cronSummaryPreview(s))
			m.cron.data.runs[0].Summary = s
			_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
			var message []string
			inMessage := false
			for _, line := range lines {
				if strings.TrimSpace(line.text) == "Recorded with" {
					break
				}
				if inMessage {
					message = append(message, strings.TrimSpace(line.text))
				}
				if strings.TrimSpace(line.text) == "Message" {
					inMessage = true
				}
			}
			require.Equal(t, tc.detail, strings.TrimSpace(strings.Join(message, "\n")))
		})
	}
}

func TestCronSummaryCacheField(t *testing.T) {
	m := cronSummaryFixture(t)
	m.width = 100
	_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
	var text strings.Builder
	for _, line := range lines {
		text.WriteString(line.text)
		text.WriteByte('\n')
	}
	require.Contains(t, text.String(), "Cache:      10 read, 20 write")
}

func TestCronSummaryGroupedEvidence(t *testing.T) {
	m := cronSummaryFixture(t)
	m.width = 180
	m.cron.data.runs[0].Summary.InputTokens = new(int64(1200))
	m.cron.data.runs[0].Summary.OutputTokens = new(int64(340))
	_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line.text)
		b.WriteByte('\n')
	}
	text := b.String()
	require.Less(t, strings.Index(text, "Log ref:"), strings.Index(text, "Usage"))
	require.Less(t, strings.Index(text, "Usage"), strings.Index(text, "Tokens:"))
	require.Contains(t, text, "  Reason:     agent_quota")
	require.Contains(t, text, "  Message:    Quota exhausted")
	require.NotContains(t, text, "Attempts:   0")
	require.Contains(t, text, "\n\n  publish · skipped")
}

func TestCronSummaryExactCostDisplay(t *testing.T) {
	for _, tc := range []struct {
		cost int64
		want string
	}{{0, "0.00 USD"}, {1, "0.000001 USD"}, {41250, "0.04125 USD"}, {1000000, "1.00 USD"}, {1234000, "1.234 USD"}} {
		m := cronSummaryFixture(t)
		m.width = 100
		m.cron.data.runs[0].Summary.Usage.CostMicros = &tc.cost
		_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
		found := false
		for _, line := range lines {
			if strings.HasPrefix(line.text, "Cost:") {
				found = true
				require.Equal(t, "Cost:       "+tc.want, line.text)
			}
		}
		require.True(t, found)
	}
}

func TestCronSummaryStepTimeEvidence(t *testing.T) {
	for _, tc := range []struct{ start, end string }{{"2026-05-01T09:00:00.0000000001Z", "2026-05-01T09:00:00.0000000002Z"}, {"2026-05-01T09:00:00.123456789Z", "2026-05-01T09:00:00.1234567891Z"}, {"2026-05-01T09:00:00Z", ""}, {"", "2026-05-01T09:00:00Z"}, {"0001-01-01T00:00:00Z", "9999-12-31T23:59:59Z"}} {
		m := cronSummaryFixture(t)
		m.width = 180
		m.cron.data.runs[0].Summary.Steps = []generated.SummaryStepResponse{{Key: "review", Status: "running", StartedAt: optionalSummaryTime(tc.start), EndedAt: optionalSummaryTime(tc.end)}}
		_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
		var b strings.Builder
		for _, line := range lines {
			b.WriteString(line.text)
			b.WriteByte('\n')
		}
		text := b.String()
		if tc.start != "" {
			require.Contains(t, text, "  Started:    "+tc.start)
		}
		if tc.end != "" {
			require.Contains(t, text, "  Ended:      "+tc.end)
		}
		require.NotContains(t, text, "Duration:")
	}
}
func optionalSummaryTime(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func TestCronSummaryHeadingAndSpacerStyle(t *testing.T) {
	applyColorMode(colorDark, true)
	defer applyDefaultColorMode()
	m := cronSummaryFixture(t)
	m.width = 100
	m.height = 80
	m = sendRune(m, '3')
	m = sendKey(m, tea.KeyEnter)
	found := false
	for line := range strings.SplitSeq(m.viewBody(), "\n") {
		plain := ansi.Strip(line)
		if strings.TrimSpace(plain) == "" {
			require.NotContains(t, line, "\x1b[48", "spacers must have no striped background")
		}
		if strings.TrimSpace(plain) == "Usage" {
			found = true
			require.Equal(t, detailSectionHeaderStyle.Render(padToWidth("  Usage", m.width)), line)
		}
	}
	require.True(t, found)
}

func TestCronSummaryCostPreservesMicrosProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		cost := hegel.Draw(ht, hegel.Integers[int64](0, math.MaxInt64))
		m := cronSummaryFixture(t)
		m.width = 100
		m.cron.data.runs[0].Summary.Usage.CostMicros = &cost
		_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
		for _, line := range lines {
			if suffix, ok := strings.CutPrefix(line.text, "Cost:"); ok {
				value := strings.TrimSuffix(strings.TrimSpace(suffix), " USD")
				whole, fraction, ok := strings.Cut(value, ".")
				require.True(ht, ok)
				require.GreaterOrEqual(ht, len(fraction), 2)
				require.LessOrEqual(ht, len(fraction), 6)
				units, err := strconv.ParseInt(whole, 10, 64)
				require.NoError(ht, err)
				micros, err := strconv.ParseInt(fraction+strings.Repeat("0", 6-len(fraction)), 10, 64)
				require.NoError(ht, err)
				require.Equal(ht, cost, units*1000000+micros)
				return
			}
		}
		ht.Fatal("known cost is missing")
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestCronSummaryFieldHangingIndent(t *testing.T) {
	d := cronDoc{width: 30, labelWidth: 11}
	d.field("Log ref", "urn:example:run-log")
	require.Len(t, d.lines, 2)
	require.Equal(t, strings.Repeat(" ", 12)+"log", d.lines[1].text)
	m := cronSummaryFixture(t)
	m.width = 32
	m.cron.data.runs[0].Summary.Steps = []generated.SummaryStepResponse{{Key: "review", Status: "running", StartedAt: new("2026-05-01T10:00:00Z")}}
	_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
	found := false
	for i, line := range lines {
		if strings.HasPrefix(line.text, "  Started:") {
			found = true
			require.Less(t, i+1, len(lines))
			require.True(t, strings.HasPrefix(lines[i+1].text, strings.Repeat(" ", 14)), lines[i+1].text)
		}
	}
	require.True(t, found)
}

func TestCronSummaryWrappedFieldStripeStableAfterScroll(t *testing.T) {
	applyColorMode(colorDark, true)
	defer applyDefaultColorMode()
	m := cronSummaryFixture(t)
	m.width, m.height = 32, 100
	m = sendRune(m, '3')
	m = sendKey(m, tea.KeyEnter)
	assertRows := func(view string) {
		first, continuation := false, false
		for line := range strings.SplitSeq(view, "\n") {
			plain := ansi.Strip(line)
			if strings.Contains(plain, "Log ref:") {
				first = true
				require.Equal(t, altRowStyle.Render(padToWidth("  Log ref:    urn:example:run-", m.width)), line)
			}
			if strings.TrimSpace(plain) == "log" {
				continuation = true
				require.Equal(t, altRowStyle.Render(padToWidth(strings.Repeat(" ", 14)+"log", m.width)), line)
			}
		}
		require.True(t, first)
		require.True(t, continuation)
	}
	assertRows(m.viewBody())
	m.height = 16
	m.cron.stack[0].cursor = 10
	assertRows(m.viewBody())
}

func TestCronSummaryStepHeadingHierarchy(t *testing.T) {
	d := cronDoc{width: 20, labelWidth: 11}
	key := strings.Repeat("review", 6)
	cronSummaryDetails(&d, generated.SummaryResponse{Version: 1, Steps: []generated.SummaryStepResponse{{Key: key, Status: "running"}}})
	var heading strings.Builder
	for _, line := range d.lines {
		if line.kind == cronHeadingLine && line.text != "Steps (1)" {
			require.True(t, strings.HasPrefix(line.text, "  "), line.text)
			require.LessOrEqual(t, ansi.StringWidth(line.text), d.width)
			heading.WriteString(strings.TrimSpace(line.text))
		}
	}
	require.Equal(t, key+"·running", strings.ReplaceAll(heading.String(), " ", ""))
}

func TestCronSummaryExactStepDuration(t *testing.T) {
	for _, start := range []string{"2026-05-01T09:00:00Z", "2026-05-01T09:00:00.0000000000Z", "2026-05-01T10:00:00.000000000000+01:00"} {
		t.Run(start, func(t *testing.T) {
			d := cronDoc{width: 180, labelWidth: 11}
			cronSummaryDetails(&d, generated.SummaryResponse{Steps: []generated.SummaryStepResponse{{Key: "review", Status: "succeeded", StartedAt: &start, EndedAt: new("2026-05-01T09:00:02Z")}}})
			var text strings.Builder
			for _, line := range d.lines {
				text.WriteString(line.text + "\n")
			}
			require.Contains(t, text.String(), "Duration:   2s")
			require.NotContains(t, text.String(), "Started:")
			require.NotContains(t, text.String(), "Ended:")
		})
	}
}
