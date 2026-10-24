// Source: go.kenn.io/kata f3a06c1 internal/tui/theme.go:119-236,
// internal/colormode/colormode.go:26-40. Board overrides are noted below.

package cron

import (
	"image/color"

	"go.kenn.io/kit/tui/markdownrender"

	"charm.land/lipgloss/v2"
)

var (
	markdownCodeBlockBg                    string
	treeCollapsed, treeExpanded            = "▸", "▾"
	titleStyle                             lipgloss.Style
	footerBarStyle                         lipgloss.Style
	subtleStyle                            lipgloss.Style
	statusStyle                            lipgloss.Style
	chipStyle                              lipgloss.Style
	selectedStyle                          lipgloss.Style
	cursorRowStyle                         lipgloss.Style
	openStyle                              lipgloss.Style
	closedStyle                            lipgloss.Style
	helpKeyStyle                           lipgloss.Style
	helpDescStyle                          lipgloss.Style
	errorStyle                             lipgloss.Style
	toastStyle                             lipgloss.Style
	chipActive                             lipgloss.Style
	tabActive                              lipgloss.Style
	tabInactive                            lipgloss.Style
	detailMetaStyle                        lipgloss.Style
	dimStyle                               lipgloss.Style
	detailSectionHeaderStyle               lipgloss.Style
	tableHeaderStyle                       lipgloss.Style
	altRowStyle                            lipgloss.Style
	passStyle                              lipgloss.Style
	failStyle                              lipgloss.Style
	attentionStyle                         lipgloss.Style
	neutralStyle                           lipgloss.Style
	cursorStyle                            lipgloss.Style
	rowSelectedStyle                       lipgloss.Style
	itemSelectedStyle                      lipgloss.Style
	headerStyle                            lipgloss.Style
	panelActiveBorder, panelInactiveBorder color.Color
)

// activeColorMode and activeHasDarkBackground record the last Apply, so a test
// can restore the palette it started with.
var (
	activeColorMode         colorMode
	activeHasDarkBackground bool
)

// applyColorMode rebuilds the palette when the terminal background changes.
func applyColorMode(mode colorMode, isDark bool) {
	activeColorMode = mode
	activeHasDarkBackground = isDark
	switch mode {
	case colorNone:
		activeHasDarkBackground = false
	case colorLight:
		activeHasDarkBackground = false
	case colorDark:
		activeHasDarkBackground = true
	}
	treeCollapsed, treeExpanded = "▸", "▾"
	base := lipgloss.NewStyle()
	if mode == colorNone {
		treeCollapsed, treeExpanded = "+", "-"
		titleStyle = base.Bold(true)
		subtleStyle = base
		statusStyle = base
		selectedStyle = base.Reverse(true)
		openStyle = base
		closedStyle = base
		helpKeyStyle = base.Bold(true)
		helpDescStyle = base
		errorStyle = base.Bold(true)
		toastStyle = base.Bold(true)
		chipStyle = base
		chipActive = base.Bold(true)
		tabActive = base.Bold(true).Underline(true)
		tabInactive = base.Faint(true)
		detailMetaStyle = base
		detailSectionHeaderStyle = base.Bold(true)
		panelActiveBorder = lipgloss.NoColor{}
		panelInactiveBorder = lipgloss.NoColor{}
		tableHeaderStyle = base.Bold(true)
		cursorRowStyle = base.Reverse(true)
		altRowStyle = base
		footerBarStyle = base.Padding(0, 1)
		markdownCodeBlockBg = ""
		dimStyle = detailMetaStyle
	} else {
		markdownCodeBlockBg = markdownrender.CodeBlockBackground(activeHasDarkBackground)
		lightDark := lipgloss.LightDark(activeHasDarkBackground)
		pick := func(light, dark string) color.Color {
			return lightDark(lipgloss.Color(light), lipgloss.Color(dark))
		}
		titleStyle = lipgloss.NewStyle().Bold(true)
		subtleStyle = lipgloss.NewStyle().Foreground(pick("242", "246"))
		statusStyle = lipgloss.NewStyle().Foreground(pick("242", "246"))
		selectedStyle = lipgloss.NewStyle().Background(pick("153", "24"))
		openStyle = lipgloss.NewStyle().Foreground(pick("28", "46"))
		closedStyle = lipgloss.NewStyle().Foreground(pick("240", "245"))
		helpKeyStyle = lipgloss.NewStyle().Foreground(pick("242", "246"))
		helpDescStyle = lipgloss.NewStyle().Foreground(pick("248", "240"))
		errorStyle = lipgloss.NewStyle().Bold(true).Foreground(pick("124", "196"))
		toastStyle = lipgloss.NewStyle().Bold(true).Foreground(pick("28", "46"))
		chipStyle = lipgloss.NewStyle().Foreground(pick("242", "246"))
		chipActive = lipgloss.NewStyle().Bold(true).Foreground(pick("125", "205"))
		tabActive = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(pick("125", "205"))
		tabInactive = lipgloss.NewStyle().Foreground(pick("242", "246"))
		detailMetaStyle = lipgloss.NewStyle()
		detailSectionHeaderStyle = lipgloss.NewStyle().Bold(true)
		panelActiveBorder = pick("125", "205")
		panelInactiveBorder = pick("242", "246")
		tableHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(pick("242", "246")).Background(pick("253", "234"))
		cursorRowStyle = lipgloss.NewStyle().Background(pick("153", "24"))
		altRowStyle = lipgloss.NewStyle().Background(pick("254", "236"))
		footerBarStyle = lipgloss.NewStyle().Padding(0, 1).Foreground(pick("242", "246")).Background(pick("253", "234"))
		dimStyle = detailMetaStyle
	}
	passStyle = openStyle
	failStyle = errorStyle
	attentionStyle = chipActive
	neutralStyle = closedStyle
	// Kata’s navigation glyph uses the row’s default foreground.
	cursorStyle = detailMetaStyle
	rowSelectedStyle = cursorRowStyle
	itemSelectedStyle = selectedStyle
	headerStyle = tableHeaderStyle
	for _, override := range kataOverrides {
		switch override.name {
		case "tabActive":
			tabActive = override.apply(tabActive, mode)
		case "headerStyle":
			headerStyle = override.apply(headerStyle, mode)
		case "dimStyle":
			dimStyle = override.apply(dimStyle, mode)
		}
	}
}

// kataOverrides records the retained product palette choices. source is the
// corresponding Kata role; issue records the decision behind the override.
var kataOverrides = []struct {
	name, source, issue string
	apply               func(lipgloss.Style, colorMode) lipgloss.Style
}{
	{"tabActive", "tabActive", "bfh54", func(s lipgloss.Style, _ colorMode) lipgloss.Style { return s.UnsetUnderline() }},
	{"headerStyle", "tableHeaderStyle", "a1p0", func(s lipgloss.Style, _ colorMode) lipgloss.Style { return s.UnsetBackground() }},
	{"dimStyle", "detailMetaStyle", "qprf", func(s lipgloss.Style, mode colorMode) lipgloss.Style {
		if mode == colorNone {
			return s
		}
		return s.Foreground(lipgloss.Color("240"))
	}},
}
