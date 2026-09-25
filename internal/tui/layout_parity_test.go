package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"go.kenn.io/kit/tui/splitlayout"
)

// Frozen renderer contract from 5266a7161e4d9fa354f73624be3d0032a80f016b.
type legacySplitGeom struct {
	ListOuterW, DetailOuterW, BodyH int
	ListInnerW, ListInnerH          int
	DetailInnerW, DetailInnerH      int
}

func legacySplitGeometry(m Model) legacySplitGeom {
	listW := m.width - 100
	listW = max(listW, 68)
	listW = min(listW, 110)
	detailW := max(m.width-listW, 20)
	bodyH := max(m.height-2-helpLines(m.splitHelpRows(), m.width), 4)
	return legacySplitGeom{listW, detailW, bodyH, listW - 2, bodyH - 2, detailW - 2, bodyH - 2}
}

func legacySplitPane(m Model, pane focusPane, width, height int) string {
	innerW, innerH := max(width-2, 10), max(height-2, 2)
	var body string
	if pane == focusList {
		chrome := m.chrome()
		chrome.narrow = true
		body = m.list.ViewBody(innerW, innerH, chrome)
	} else {
		body = splitDetailBody(m, innerW, innerH)
	}
	border := panelInactiveBorder
	if m.focus == pane {
		border = panelActiveBorder
	}
	return lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(border).Width(width).Height(height).Render(body)
}

func legacySplitDetailIndicator(m Model) string {
	if m.detail.issue == nil {
		return ""
	}
	g := legacySplitGeometry(m)
	innerW, innerH := max(g.DetailInnerW, 10), max(g.DetailInnerH, 2)
	lines, _ := m.detail.detailDocumentLines(innerW, m.chrome())
	scroll := clampScroll(m.detail.scroll, len(lines), innerH)
	return documentScrollIndicator(len(lines), scroll, innerH)
}

func legacySplitInfoLine(m Model) string {
	chrome := m.chrome()
	var body string
	switch {
	case chrome.input.kind.isCommandBar():
		body = renderInfoBar(chrome.input, titleBarInnerWidth(m.width))
	case chrome.input.kind.isPanelPrompt():
		body = renderInfoPrompt(chrome.input, titleBarInnerWidth(m.width))
	case m.focus == focusList && m.list.status != "":
		body = m.list.status
	case m.focus == focusDetail && m.detail.status != "":
		body = m.detail.status
	case chrome.sseStatus != sseConnected:
		body = sseDegradedFlash(chrome.sseStatus)
	case chrome.toast != nil:
		body = chrome.toast.text
	case m.focus == focusList:
		body = rightAlignInside(footerPositionIndicator(m.list.cursor, len(m.list.visibleRows())), titleBarInnerWidth(m.width))
	case m.focus == focusDetail:
		body = rightAlignInside(legacySplitDetailIndicator(m), titleBarInnerWidth(m.width))
	}
	return statsLineStyle.Render(padToWidth(body, titleBarInnerWidth(m.width)))
}

func legacyRenderSplit(m Model) string {
	g := legacySplitGeometry(m)
	chrome := m.chrome()
	title := renderTitleBar(m.width, chrome.scope, chrome.version, chrome.daemon)
	list := legacySplitPane(m, focusList, g.ListOuterW, g.BodyH)
	detail := legacySplitPane(m, focusDetail, g.DetailOuterW, g.BodyH)
	body := lipgloss.JoinHorizontal(lipgloss.Top, list, detail)
	return strings.Join([]string{title, body, legacySplitInfoLine(m), renderSplitFooter(m.width, m)}, "\n")
}

func splitParityModel(width, height int, focus focusPane) Model {
	m := snapSplitModel(width, height, focus)
	m.scope.projectName = "example-project"
	m.list.actor = "example-actor"
	return m
}

func assertSplitBytes(t *testing.T, label, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	t.Fatalf("%s differs at byte %d; lengths got=%d want=%d; got=%q want=%q", label, i, len(got), len(want), got[i:min(i+80, len(got))], want[i:min(i+80, len(want))])
}

// Widths 87/88/89 straddle the detail floor; height 40 covers a normal frame.
var splitParityWidths = []int{0, 40, 87, 88, 89, 139, 140, 141, 167, 168, 169, 180, 209, 210, 211, 300}
var splitParityHeights = []int{0, 4, 35, 36, 37, 40, 50}

func TestSplitLayout_RenderParity(t *testing.T) { //nolint:paralleltest // sets KATA_COLOR_MODE; applyColorMode rewrites package style vars
	defer snapshotInit(t)()
	for _, palette := range []struct {
		name string
		mode colorMode
	}{{"none", colorNone}, {"light", colorLight}, {"dark", colorDark}} {
		t.Run(palette.name, func(t *testing.T) {
			t.Setenv("KATA_COLOR_MODE", palette.name)
			defer applyDefaultColorMode()
			for _, width := range splitParityWidths {
				for _, height := range splitParityHeights {
					for _, focus := range []focusPane{focusList, focusDetail} {
						for _, empty := range []bool{false, true} {
							for _, input := range []bool{false, true} {
								name := fmt.Sprintf("%dx%d/focus=%d/empty=%t/input=%t", width, height, focus, empty, input)
								t.Run(name, func(t *testing.T) {
									m := splitParityModel(width, height, focus)
									applyColorMode(palette.mode, false)
									if empty {
										m.detail.issue = nil
									}
									if input {
										m.input = newSearchBar(m.list.filter)
									}
									if width >= 140 && height >= 36 {
										assertSplitBytes(t, "viewContent", m.viewContent(), legacyRenderSplit(m))
									} else {
										assertSplitBytes(t, "list pane", renderSplitListPane(m, width, height), legacySplitPane(m, focusList, width, height))
										assertSplitBytes(t, "detail pane", renderSplitDetailPane(m, width, height), legacySplitPane(m, focusDetail, width, height))
									}
								})
							}
						}
					}
				}
			}
		})
	}
}

func TestSplitLayout_GeometryParity(t *testing.T) { //nolint:paralleltest // snapshotInit sets KATA_COLOR_MODE and NO_COLOR; applyColorMode rewrites package style vars
	defer snapshotInit(t)()
	for _, width := range splitParityWidths {
		for _, height := range splitParityHeights {
			t.Run(fmt.Sprintf("%dx%d", width, height), func(t *testing.T) {
				for _, focus := range []focusPane{focusList, focusDetail} {
					for _, input := range []bool{false, true} {
						m := splitParityModel(width, height, focus)
						if input {
							m.input = newSearchBar(m.list.filter)
						}
						wantMode := splitlayout.Stacked
						if width >= 140 && height >= 36 {
							wantMode = splitlayout.Split
						}
						if got := m.resolveLayout(); got != wantMode {
							t.Fatalf("mode=%v, want %v", got, wantMode)
						}
						got := legacySplitGeom(m.splitGeometry())
						if want := legacySplitGeometry(m); got != want {
							t.Fatalf("focus=%d input=%t geometry=%+v, want %+v", focus, input, got, want)
						}
						m.layout = splitlayout.Split
						assertSplitConsumers(t, m)
						m.layout = splitlayout.Stacked
						dm := m.cacheDetailViewport(m.detail)
						if dm.lastDetailSplit || dm.lastDetailWidth != 0 || dm.lastDetailHeight != 0 {
							t.Fatal("stacked layout retained split viewport dimensions")
						}
					}
				}
			})
		}
	}
}

func assertSplitConsumers(t *testing.T, m Model) {
	t.Helper()
	g := legacySplitGeometry(m)
	dm := m.applyDetailViewportCache(m.detail)
	if !dm.lastDetailSplit || dm.lastDetailWidth != max(g.DetailInnerW, 10) || dm.lastDetailHeight != max(g.DetailInnerH, 2) {
		t.Fatalf("viewport=%dx%d split=%t, legacy=%+v", dm.lastDetailWidth, dm.lastDetailHeight, dm.lastDetailSplit, g)
	}
	wantRows := max(1, max(g.ListInnerH, 2)-1)
	if m.width <= 0 || m.height <= 0 {
		wantRows = 0
	}
	if got := m.listRenderedDataRows(); got != wantRows {
		t.Fatalf("paging rows=%d, want %d", got, wantRows)
	}
	if got, want := m.listDataBudget(), max(g.ListInnerH, 2)-2; got != want {
		t.Fatalf("mouse rows=%d, want %d", got, want)
	}
}

func TestLayout_ResolveLayout_Unlocked_Thresholds(t *testing.T) { //nolint:paralleltest // snapshotInit sets KATA_COLOR_MODE and NO_COLOR; applyColorMode rewrites package style vars
	defer snapshotInit(t)()
	for _, tc := range []struct {
		width, height int
		mode          splitlayout.Mode
	}{{139, 36, splitlayout.Stacked}, {140, 35, splitlayout.Stacked}, {140, 36, splitlayout.Split}} {
		t.Run(fmt.Sprintf("%dx%d", tc.width, tc.height), func(t *testing.T) {
			m := splitParityModel(tc.width, tc.height, focusList)
			m, _ = updateModel(m, tea.WindowSizeMsg{Width: tc.width, Height: tc.height})
			if m.layout != tc.mode || m.layoutLocked {
				t.Fatalf("layout=%v locked=%t, want %v unlocked", m.layout, m.layoutLocked, tc.mode)
			}
			want := m.list.View(m.width, m.height, m.chrome())
			if tc.mode == splitlayout.Split {
				want = legacyRenderSplit(m)
			}
			assertSplitBytes(t, "threshold frame", m.viewContent(), want)
		})
	}
}

func TestSplitLayout_ConsumerGeometry(t *testing.T) { //nolint:paralleltest // snapshotInit sets KATA_COLOR_MODE and NO_COLOR; applyColorMode rewrites package style vars
	defer snapshotInit(t)()
	for _, width := range []int{168, 180, 211} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			m := splitParityModel(width, 40, focusDetail)
			m.opts.Mouse = true
			m.detail.issue.Body = strings.Repeat("A long detail document with words that wrap at the pane edge.\n\n", 60)
			m.detail.scroll = 5
			footerHeights := map[int]bool{}
			for _, state := range []string{"detail", "list", "input", "restored"} {
				t.Run(state, func(t *testing.T) {
					m.input = inputState{}
					m.focus = focusDetail
					if state == "list" {
						m.focus = focusList
					}
					if state == "input" {
						m.input = newSearchBar(m.list.filter)
					}
					g := legacySplitGeometry(m)
					footerHeights[helpLines(m.splitHelpRows(), width)] = true
					assertSplitConsumers(t, m)
					m.detail = m.applyDetailViewportCache(m.detail)
					lines, _ := m.detail.detailDocumentLines(max(g.DetailInnerW, 10), m.chrome())
					for _, scroll := range []int{5, len(lines) - g.DetailInnerH - 1} {
						m.detail.scroll = scroll
						want := fmt.Sprintf("[lines %d-%d of %d]", scroll+1, scroll+g.DetailInnerH, len(lines))
						if scroll < 1 || scroll+g.DetailInnerH >= len(lines) {
							t.Fatal("fixture must scroll within a longer document")
						}
						if got := splitDetailScrollIndicator(m); got != want {
							t.Fatalf("indicator=%q, want %q", got, want)
						}
						assertSplitBytes(t, "scrolling frame", m.viewContent(), legacyRenderSplit(m))
					}
					t.Logf("footer=%d list=%d detail=%d body=%d viewport=%dx%d", helpLines(m.splitHelpRows(), width), g.ListOuterW, g.DetailOuterW, g.BodyH, m.detail.lastDetailWidth, m.detail.lastDetailHeight)
				})
			}
			if len(footerHeights) < 2 {
				t.Fatal("fixture must change footer height")
			}
			m.input = inputState{}
			m.detail.scroll = 5
			g := legacySplitGeometry(m)
			pane := renderSplitListPane(m, g.ListOuterW, g.BodyH)
			if edge := lipgloss.Width(pane); edge != g.ListOuterW {
				t.Fatalf("rendered list edge=%d, want %d", edge, g.ListOuterW)
			}
			for _, x := range []int{g.ListOuterW - 1, g.ListOuterW} {
				t.Run(fmt.Sprintf("mouse-x=%d", x), func(t *testing.T) {
					clicked, _ := updateModel(m, mouseLeftClick(x, 4))
					wheeled, _ := updateModel(m, mouseWheelDownAt(x))
					if x < g.ListOuterW {
						if clicked.focus != focusList || clicked.list.cursor != 0 || wheeled.focus != focusList || wheeled.list.cursor != 2 {
							t.Fatalf("list edge click=%d/%d wheel=%d/%d", clicked.focus, clicked.list.cursor, wheeled.focus, wheeled.list.cursor)
						}
					} else if clicked.focus != focusDetail || clicked.list.cursor != 1 || clicked.detail.scroll != 5 || wheeled.focus != focusDetail || wheeled.list.cursor != 1 || wheeled.detail.scroll != 8 {
						t.Fatalf("detail edge click=%d/%d/%d wheel=%d/%d/%d", clicked.focus, clicked.list.cursor, clicked.detail.scroll, wheeled.focus, wheeled.list.cursor, wheeled.detail.scroll)
					}
				})
			}
			m.input = newPanelPrompt(inputLabelPrompt, formTarget{projectID: 7, issueShortID: m.detail.issue.ShortID})
			m.projectLabels.byProject[7] = labelCacheEntry{pid: 7, gen: 1, labels: []LabelCount{{Label: "example-label", Count: 1}}}
			menu := renderSuggestMenu(m.input, m.suggestionsForPrompt(m.input), m.cacheEntryForPrompt(m.input))
			row, col := m.height-2-lipgloss.Height(menu), max(m.width-lipgloss.Width(menu)-1, g.ListOuterW+1)
			want := overlayAtCorner(legacyRenderSplit(m), menu, m.width, m.height, row, col)
			assertSplitBytes(t, "suggestion position", m.viewContent(), want)
			t.Logf("suggestion row=%d col=%d list edge=%d", row, col, g.ListOuterW)
		})
	}
}
