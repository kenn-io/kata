package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
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
	m.attention.rows[2].workAttention = "ok"
	m.attention.skipped = 1
	return m
}

func TestAttentionThemeRoles(t *testing.T) {
	defer snapshotInit(t)()
	for _, theme := range []struct {
		name string
		mode colorMode
	}{{"light", colorLight}, {"dark", colorDark}, {"none", colorNone}} {
		mode := theme.mode
		t.Run(theme.name, func(t *testing.T) {
			m := attentionSnapshot(t)
			applyColorMode(mode, mode == colorDark)
			// Keep every semantic value off the cursor, then move onto it.
			m.attention.rows = append(m.attention.rows, attentionRequest{issue: testIssue("mno8")})
			m.attention.cursor = 4
			lines := renderAttentionRows(m.attention, 120, 30)
			for i, cell := range []struct {
				text  string
				style lipgloss.Style
			}{
				{"needs-human", chipActive}, {"stuck", errorStyle}, {"ok", subtleStyle}, {"—", subtleStyle},
			} {
				rowStyle := normalRowStyle
				if i%2 == 1 {
					rowStyle = altRowStyle
				}
				require.Contains(t, lines[i+1], strings.TrimSuffix(cell.style.Inherit(rowStyle).Render(cell.text), "\x1b[m"))
			}
			require.Contains(t, lines[0], tableHeaderStyle.Render("attention"))
			// Plain title/owner cells inherit only the issue-list row background.
			require.Contains(t, lines[2], strings.TrimSuffix(altRowStyle.Render("issue def5"), "\x1b[m"))
			require.Contains(t, lines[2], strings.TrimSuffix(altRowStyle.Render("system"), "\x1b[m"))
			require.Contains(t, lines[5], strings.TrimSuffix(cursorRowStyle.Render("#mno8"), "\x1b[m"))
		})
	}
}

func TestSnapshot_Attention_80x24(t *testing.T) {
	defer snapshotInit(t)()
	m := attentionSnapshot(t)
	m.width, m.height = 80, 24
	assertGolden(t, "attention-80x24", renderAttention(m))
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

func TestAttentionFreshEntryClearsPreviousSkippedCount(t *testing.T) {
	m, _ := attentionFixture(t)
	m.attention.skipped = 1
	m, cmd := m.transitionToAttention()
	require.NotNil(t, cmd)

	plain := stripANSI(renderAttention(m))
	require.Contains(t, plain, "Loading attention requests…")
	require.NotContains(t, plain, "1 skipped",
		"a fresh loading view must not show the previous result set's skipped count")
}

func TestAttentionColumnsAlignSelectionMarker(t *testing.T) {
	defer snapshotInit(t)()
	m := attentionSnapshot(t)
	m.attention.allProjects = false
	rows := renderAttentionRows(m.attention, m.width, 30)

	selectedID := "#" + m.attention.rows[0].issue.ShortID
	unselectedID := "#" + m.attention.rows[1].issue.ShortID
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
	selectedCells := []string{selectedID, m.attention.rows[0].issue.Title, "sam", "needs-human"}
	unselectedCells := []string{unselectedID, m.attention.rows[1].issue.Title, "system", "stuck"}
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
			require.Contains(t, stripANSI(out), "message 1–")
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

// Inspect the terminal's effective attributes, including resets and padding;
// finding a styled substring alone cannot detect a short band or leaked color.
type attentionCellAttributes struct {
	foreground, background string
	bold, faint, reverse   bool
}

func attentionTerminalCells(text string) []attentionCellAttributes {
	var cells []attentionCellAttributes
	var current attentionCellAttributes
	parser := ansi.NewParser()
	parser.SetHandler(ansi.Handler{
		Print: func(r rune) {
			for range ansi.StringWidth(string(r)) {
				cells = append(cells, current)
			}
		},
		HandleCsi: func(cmd ansi.Cmd, params ansi.Params) {
			if cmd != 'm' {
				return
			}
			if len(params) == 0 {
				current = attentionCellAttributes{}
				return
			}
			for i := 0; i < len(params); i++ {
				v := params[i].Param(0)
				switch v {
				case 0:
					current = attentionCellAttributes{}
				case 1:
					current.bold = true
				case 2:
					current.faint = true
				case 22:
					current.bold = false
					current.faint = false
				case 7:
					current.reverse = true
				case 27:
					current.reverse = false
				case 39:
					current.foreground = ""
				case 49:
					current.background = ""
				case 38, 48:
					if i+2 < len(params) && params[i+1].Param(0) == 5 {
						color := fmt.Sprint(params[i+2].Param(0))
						if v == 38 {
							current.foreground = color
						} else {
							current.background = color
						}
						i += 2
					}
				}
			}
		},
	})
	parser.Parse([]byte(text))
	return cells
}

func TestAttentionFullWidthBandsAndPlainCells(t *testing.T) {
	defer snapshotInit(t)()
	for _, theme := range []struct {
		name string
		mode colorMode
	}{{"light", colorLight}, {"dark", colorDark}, {"none", colorNone}} {
		for _, width := range []int{80, 120} {
			t.Run(fmt.Sprintf("%s/%d", theme.name, width), func(t *testing.T) {
				m := attentionSnapshot(t)
				m.attention.rows[0].from = "casey"
				m.attention.rows[1].from = "system"
				m.attention.rows[1].teammate = "cron"
				applyColorMode(theme.mode, theme.mode == colorDark)
				for cursor := range m.attention.rows {
					m.attention.cursor = cursor
					lines := renderAttentionRows(m.attention, width, 30)
					header := stripANSI(lines[0])
					attentionStart, titleStart := strings.Index(header, "attention"), strings.Index(header, "title")
					for i := range m.attention.rows {
						cells := attentionTerminalCells(lines[i+1])
						require.Len(t, cells, width, "every row includes padding through the owner column")
						rowStyle := normalRowStyle
						if i == cursor {
							rowStyle = cursorRowStyle
						} else if i%2 == 1 {
							rowStyle = altRowStyle
						}
						expected := attentionTerminalCells(rowStyle.Render("x"))[0]
						for x, cell := range cells {
							require.Equal(t, expected.background, cell.background, "row %d column %d must retain its band", i, x)
							require.Equal(t, expected.reverse, cell.reverse, "row %d column %d must retain reverse selection", i, x)
							if x < attentionStart || x >= titleStart {
								require.Empty(t, cell.foreground, "attention %q must not color row %d column %d", m.attention.rows[i].workAttention, i, x)
								require.False(t, cell.faint, "attention %q must not dim row %d column %d", m.attention.rows[i].workAttention, i, x)
								require.False(t, cell.bold, "attention %q must not bold row %d column %d", m.attention.rows[i].workAttention, i, x)
							}
						}
					}
				}
			})
		}
	}
}
