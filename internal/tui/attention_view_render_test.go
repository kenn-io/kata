package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func attentionSnapshot(t *testing.T) Model {
	t.Helper()
	m, a := attentionFixture(t)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	a.rows = append(a.rows, attentionRequest{issue: testIssue("ghi6"), from: "system", teammate: "cron", message: "Weekly review ready"}, attentionRequest{issue: testIssue("jkl7"), from: "sam", teammate: "worker", message: "Please check"})
	m.attention.rows = a.rows
	m.attention.rows[0].message = strings.Repeat("Please review the retry behavior before the next run. ", 12)
	m.attention.rows[0].workAttention = "needs-human"
	m.attention.rows[1].workAttention = "stuck"
	m.attention.skipped = 1
	return m
}
func TestSnapshot_Attention_Populated(t *testing.T) {
	defer snapshotInit(t)()
	m := attentionSnapshot(t)
	assertGolden(t, "attention-populated", renderAttention(m))
}
func TestSnapshot_Attention_Empty(t *testing.T) {
	defer snapshotInit(t)()
	m := attentionSnapshot(t)
	m.attention.rows = nil
	m.attention.skipped = 0
	m.attention.allProjects = true
	assertGolden(t, "attention-empty", renderAttention(m))
}

func TestAttentionColumnsAlignSelectionMarker(t *testing.T) {
	defer snapshotInit(t)()
	m := attentionSnapshot(t)
	m.attention.allProjects = false
	rows := renderAttentionRows(m.attention, m.width, 30)

	selectedID := m.attention.rows[0].issue.ShortID
	unselectedID := m.attention.rows[1].issue.ShortID
	selectedLine, unselectedLine := "", ""
	for _, line := range rows {
		plain := stripANSI(line)
		if strings.Contains(plain, selectedID) {
			selectedLine = line
		}
		if strings.Contains(plain, unselectedID) {
			unselectedLine = line
		}
	}
	require.NotEmpty(t, selectedLine)
	require.NotEmpty(t, unselectedLine)
	selectedCells := []string{selectedID, "issue " + selectedID, "sam", "needs-human"}
	unselectedCells := []string{unselectedID, "issue " + unselectedID, "system", "stuck"}
	for i := range selectedCells {
		selectedIndex := strings.Index(selectedLine, selectedCells[i])
		unselectedIndex := strings.Index(unselectedLine, unselectedCells[i])
		require.NotEqual(t, -1, selectedIndex)
		require.NotEqual(t, -1, unselectedIndex)
		selectedOffset := ansi.StringWidth(selectedLine[:selectedIndex])
		unselectedOffset := ansi.StringWidth(unselectedLine[:unselectedIndex])
		require.Equal(t, unselectedOffset, selectedOffset,
			"selected and unselected cells must start in the same column")
		if i == 0 {
			require.Equal(t, listColumnWidths(m.width, false).nav, selectedOffset,
				"attention rows must reserve the same navigation gutter as kata tui's issue list")
		}
	}
}

func TestSnapshot_Attention_Error(t *testing.T) {
	defer snapshotInit(t)()
	m := attentionSnapshot(t)
	m.attention.rows = nil
	m.attention.err = errors.New("boom")
	assertGolden(t, "attention-error", renderAttention(m))
}
func TestAttentionRenderBounds(t *testing.T) {
	defer snapshotInit(t)()
	m := attentionSnapshot(t)
	m.attention.rows[0].message = "\x1b[2J" + strings.Repeat("review ", 300)
	for _, size := range [][2]int{{120, 40}, {80, 24}, {80, 8}} {
		m.width, m.height = size[0], size[1]
		out := renderAttention(m)
		require.NotContains(t, out, "\x1b[2J")
		require.LessOrEqual(t, len(strings.Split(out, "\n")), m.height)
		for line := range strings.SplitSeq(out, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), m.width)
		}
		if m.height >= 24 {
			require.Contains(t, stripANSI(out), "…")
		}
	}
	m.attention.rows = nil
	m.attention.recipient = "alex/reviewer"
	m.attention.allProjects = true
	m.height = 40
	require.Contains(t, stripANSI(renderAttention(m)), "No requests for alex/reviewer across all projects")
	m.attention.err = errors.New("boom")
	require.Contains(t, stripANSI(renderAttention(m)), "failed to load attention requests: boom")
	m.width = 79
	require.Contains(t, stripANSI(m.viewBody()), "attention") // full View supplies the common narrow hint.
	require.Contains(t, stripANSI(m.View().Content), "needs more space")
}

func TestAttentionMessageTabs(t *testing.T) {
	defer snapshotInit(t)()
	m := attentionSnapshot(t)
	m.attention.rows[0].message = "Review\tthis\trequest"
	out := renderAttention(m)
	require.NotContains(t, out, "\t")
	require.Contains(t, stripANSI(out), "Review    this    request")
}
