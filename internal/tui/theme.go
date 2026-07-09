package tui

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

type colorMode int

const (
	colorAuto colorMode = iota
	colorDark
	colorLight
	colorNone
)

// resolveColorMode honors NO_COLOR (any non-empty value) over
// KATA_COLOR_MODE. Unrecognized values fall back to auto.
func resolveColorMode() colorMode {
	if v := os.Getenv("NO_COLOR"); v != "" {
		return colorNone
	}
	switch strings.ToLower(os.Getenv("KATA_COLOR_MODE")) {
	case "dark":
		return colorDark
	case "light":
		return colorLight
	case "none":
		return colorNone
	default:
		return colorAuto
	}
}

// Style vars are package-level so View() functions don't reach into
// state. applyColorMode rebuilds them once at boot.
//
// The palette mirrors roborev's (cmd/roborev/tui/tui.go:38-77) so the
// two TUIs feel consistent. Where kata's status semantics differ from
// roborev's, the colors are remapped: openStyle reuses roborev's
// passStyle (green), closedStyle is neutral gray so warm/night-shift
// displays don't collapse blue/cyan into green, and deletedStyle reuses
// roborev's failStyle (red) with Faint so deleted rows read as
// out-of-band rather than alarming.
var (
	titleStyle               lipgloss.Style
	subtleStyle              lipgloss.Style
	statusStyle              lipgloss.Style
	selectedStyle            lipgloss.Style
	openStyle                lipgloss.Style
	closedStyle              lipgloss.Style
	deletedStyle             lipgloss.Style
	helpKeyStyle             lipgloss.Style
	helpDescStyle            lipgloss.Style
	errorStyle               lipgloss.Style
	toastStyle               lipgloss.Style
	chipStyle                lipgloss.Style
	chipActive               lipgloss.Style
	tabActive                lipgloss.Style
	tabInactive              lipgloss.Style
	detailMetaStyle          lipgloss.Style
	detailSectionHeaderStyle lipgloss.Style
	markdownCodeBlockStyle   lipgloss.Style
)

var (
	activeColorMode         = colorAuto
	activeHasDarkBackground bool
)

// Border colors used by M3+ render code for panel chrome (focused vs
// unfocused panes, form/prompt boxes). Stored as color.Color so callers
// pass them straight to BorderForeground without re-resolving the color
// mode. Re-bound by applyColorMode so KATA_COLOR_MODE picks the right
// shade.
var (
	panelActiveBorder   color.Color // magenta
	panelInactiveBorder color.Color // gray
)

// markdownCodeBlockBg is the glamour-facing code-block background as an
// ANSI-256 palette string ("" = no background). Kept alongside
// markdownCodeBlockStyle because glamour's StyleConfig wants a *string,
// not a color.Color — re-deriving the string from the style would need
// a reverse color→string mapping. Re-bound by applyColorMode.
var markdownCodeBlockBg string

// M3.5 chrome styles — borrowed from msgvault's view.go palette. Each
// pairs a foreground/bold/italic with an adaptive background so the
// rendered cell visibly differs from the surrounding void (msgvault's
// pattern; helps the chrome read as window furniture, not stray text).
//
// titleBarStyle: the top brand strip. Bold + adaptive bg + horizontal
//
//	padding so the bar looks like a window-chrome strip.
//
// statsStyle: second header line + info line. Faint foreground.
// tableHeaderStyle: column headers above the table body, with a
// background in color modes.
// separatorRuleStyle: subtle rules used by detail/chrome renderers.
// cursorRowStyle: highlighted background for the row under the cursor.
// altRowStyle: subtle alternate background for odd rows.
// normalRowStyle: explicit background for even rows so partial-line
//
//	updates don't leave artifacts on terminals that retain prior content.
//
// footerBarStyle: the persistent footer help row at the bottom.
// modalBoxStyle: rounded-bordered overlay box for confirm/info modals.
var (
	titleBarStyle      lipgloss.Style
	statsLineStyle     lipgloss.Style
	tableHeaderStyle   lipgloss.Style
	separatorRuleStyle lipgloss.Style
	cursorRowStyle     lipgloss.Style
	altRowStyle        lipgloss.Style
	normalRowStyle     lipgloss.Style
	footerBarStyle     lipgloss.Style
	modalBoxStyle      lipgloss.Style
)

// applyColorMode rebuilds all package-level styles. Called at TUI boot
// and again when the terminal answers the in-loop background-color
// query (tea.BackgroundColorMsg), so tests can swap modes without
// leaking state across tests. isDark is only consulted in colorAuto;
// the explicit modes pin it. Lip Gloss v2 has no renderer — color
// downsampling happens in Bubble Tea's compositor against the real
// output stream, so no capability probe runs here.
func applyColorMode(m colorMode, isDark bool) {
	activeColorMode = m
	activeHasDarkBackground = isDark
	if m == colorNone {
		activeHasDarkBackground = false
		base := lipgloss.NewStyle()
		titleStyle = base.Bold(true)
		subtleStyle = base
		statusStyle = base
		selectedStyle = base.Reverse(true)
		openStyle = base
		closedStyle = base
		deletedStyle = base.Faint(true)
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
		markdownCodeBlockStyle = base
		markdownCodeBlockBg = ""
		// Borders carry no foreground in colorNone — lipgloss renders
		// them in the default terminal color. NoColor is the closest
		// stand-in for "use whatever the terminal would otherwise pick."
		panelActiveBorder = lipgloss.NoColor{}
		panelInactiveBorder = lipgloss.NoColor{}
		// M3.5 chrome under colorNone: no backgrounds (snapshots stay
		// plain text), Bold/Faint preserved so structure reads even
		// when colors are stripped.
		titleBarStyle = base.Bold(true).Padding(0, 1)
		statsLineStyle = base.Padding(0, 1)
		tableHeaderStyle = base.Bold(true)
		separatorRuleStyle = base.Faint(true)
		cursorRowStyle = base.Reverse(true)
		altRowStyle = base
		normalRowStyle = base
		footerBarStyle = base.Padding(0, 1)
		modalBoxStyle = base.Border(lipgloss.RoundedBorder()).Padding(1, 2)
		return
	}
	switch m {
	case colorLight:
		activeHasDarkBackground = false
	case colorDark:
		activeHasDarkBackground = true
	}
	// Lip Gloss v2 removed AdaptiveColor (adaptive-at-render-time). The
	// light/dark decision resolves here instead: applyColorMode re-runs
	// when tea.BackgroundColorMsg lands, so every style rebuilds against
	// the terminal's actual background.
	lightDark := lipgloss.LightDark(activeHasDarkBackground)
	pick := func(light, dark string) color.Color {
		return lightDark(lipgloss.Color(light), lipgloss.Color(dark))
	}
	// titleStyle is bold without a saturated foreground so the issue
	// header reads as the page lead rather than a magenta announcement.
	// The full chrome stack (project bar accent, status pill, focused
	// borders) still carries hue; the title leans on weight + position
	// to be the visual anchor.
	titleStyle = lipgloss.NewStyle().Bold(true)
	subtleStyle = lipgloss.NewStyle().Foreground(pick("242", "246"))
	statusStyle = lipgloss.NewStyle().Foreground(pick("242", "246"))
	selectedStyle = lipgloss.NewStyle().Background(pick("153", "24"))
	openStyle = lipgloss.NewStyle().Foreground(pick("28", "46"))
	closedStyle = lipgloss.NewStyle().Foreground(pick("240", "245"))
	// deletedStyle is the dim-red semantic remap of roborev's failStyle
	// — design doc §"Visual language". Faint avoids reading as alarming
	// while still distinguishing soft-deleted rows from open/closed.
	deletedStyle = lipgloss.NewStyle().Faint(true).Foreground(pick("124", "196"))
	helpKeyStyle = lipgloss.NewStyle().Foreground(pick("242", "246"))
	helpDescStyle = lipgloss.NewStyle().Foreground(pick("248", "240"))
	errorStyle = lipgloss.NewStyle().Bold(true).Foreground(pick("124", "196"))
	toastStyle = lipgloss.NewStyle().Bold(true).Foreground(pick("28", "46"))
	chipStyle = lipgloss.NewStyle().Foreground(pick("242", "246"))
	chipActive = lipgloss.NewStyle().Bold(true).Foreground(pick("125", "205"))
	tabActive = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(pick("125", "205"))
	tabInactive = lipgloss.NewStyle().Foreground(pick("242", "246"))
	// detailMetaStyle and detailSectionHeaderStyle no longer paint a
	// background band. The earlier full-width slabs read as heavy
	// chrome that competed with the issue body. Section labels lean on
	// bold weight + position; metadata rows lean on subtleStyle for
	// the label half and plain text for the value half.
	detailMetaStyle = lipgloss.NewStyle()
	detailSectionHeaderStyle = lipgloss.NewStyle().Bold(true)
	markdownCodeBlockStyle = lipgloss.NewStyle().Background(pick("252", "236"))
	if activeHasDarkBackground {
		markdownCodeBlockBg = "236"
	} else {
		markdownCodeBlockBg = "252"
	}
	panelActiveBorder = pick("125", "205")
	panelInactiveBorder = pick("242", "246")
	// M3.5 chrome — adaptive bgs lifted from msgvault. titleBar uses a
	// brighter bg than statsLine so the brand strip stands out from
	// the breadcrumb row. cursorRow is brighter than altRow so the
	// three-tier striping reads cleanly even in 256-color terminals.
	titleBarStyle = lipgloss.NewStyle().Bold(true).Padding(0, 1).
		Foreground(pick("232", "255")).
		Background(pick("248", "238"))
	statsLineStyle = lipgloss.NewStyle().Padding(0, 1).
		Foreground(pick("242", "246")).
		Background(pick("253", "234"))
	tableHeaderStyle = lipgloss.NewStyle().Bold(true).
		Foreground(pick("242", "246")).
		Background(pick("253", "234"))
	separatorRuleStyle = lipgloss.NewStyle().Faint(true).
		Foreground(pick("248", "242"))
	cursorRowStyle = lipgloss.NewStyle().
		Background(pick("153", "24"))
	altRowStyle = lipgloss.NewStyle().
		Background(pick("254", "236"))
	normalRowStyle = lipgloss.NewStyle()
	footerBarStyle = lipgloss.NewStyle().Padding(0, 1).
		Foreground(pick("242", "246")).
		Background(pick("253", "234"))
	modalBoxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2).
		BorderForeground(pick("125", "205"))
}

// applyDefaultColorMode populates the style vars at boot. Before the
// terminal has answered the background-color query the dark palette is
// assumed (the common terminal default, and v1 termenv's fallback);
// Update re-runs applyColorMode with the real answer when
// tea.BackgroundColorMsg lands, so an auto-mode light terminal restyles
// on the first frame after the response.
func applyDefaultColorMode() { applyColorMode(resolveColorMode(), true) }
