package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestResolveColorMode_NoColorOverridesAll(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("KATA_COLOR_MODE", "dark")
	if got := resolveColorMode(); got != colorNone {
		t.Fatalf("NO_COLOR=1 must force colorNone, got %v", got)
	}
}

func TestResolveColorMode_KataColorModeRespected(t *testing.T) {
	cases := map[string]colorMode{
		"":      colorAuto,
		"auto":  colorAuto,
		"dark":  colorDark,
		"light": colorLight,
		"none":  colorNone,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			t.Setenv("KATA_COLOR_MODE", in)
			if got := resolveColorMode(); got != want {
				t.Fatalf("KATA_COLOR_MODE=%q -> %v, want %v", in, got, want)
			}
		})
	}
}

func TestResolveColorMode_InvalidFallsBackToAuto(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("KATA_COLOR_MODE", "rainbow")
	if got := resolveColorMode(); got != colorAuto {
		t.Fatalf("invalid value should fall back to colorAuto, got %v", got)
	}
}

func TestApplyColorMode_NoneStripsForeground(t *testing.T) {
	applyColorMode(colorNone, false)
	// Lip Gloss v2 styles always emit attribute sequences (bold etc.);
	// colorNone's contract is that no COLOR is set — the text content
	// must survive an ANSI strip unchanged and carry no foreground.
	if fg := titleStyle.GetForeground(); fg != nil && fg != (lipgloss.NoColor{}) {
		t.Fatalf("colorNone must not set a foreground, got %v", fg)
	}
	rendered := titleStyle.Render("hello")
	if got := stripANSI(rendered); got != "hello" {
		t.Fatalf("colorNone should render plain text after ANSI strip, got %q", got)
	}
	if strings.Contains(rendered, "\x1b[3") || strings.Contains(rendered, "\x1b[9") ||
		strings.Contains(rendered, "38;") {
		t.Fatalf("colorNone rendered a color sequence: %q", rendered)
	}
}

// TestApplyColorMode_RebuildsAllStyles guards against silently
// forgetting a style var in applyColorMode (which would leak the prior
// mode's value across boots). We pre-poison every var with a sentinel
// foreground (a concrete RGB color) so that GetForeground returns
// that exact value. After applyColorMode(colorNone) every var must
// have shed the sentinel foreground (colorNone leaves Foreground unset
// or a different value entirely).
func TestApplyColorMode_RebuildsAllStyles(t *testing.T) {
	sentinelColor := lipgloss.Color("#0f0f0f")
	sentinel := lipgloss.NewStyle().Foreground(sentinelColor)
	titleStyle = sentinel
	subtleStyle = sentinel
	statusStyle = sentinel
	selectedStyle = sentinel
	openStyle = sentinel
	closedStyle = sentinel
	deletedStyle = sentinel
	helpKeyStyle = sentinel
	helpDescStyle = sentinel
	errorStyle = sentinel
	toastStyle = sentinel
	chipStyle = sentinel
	chipActive = sentinel
	tabActive = sentinel
	tabInactive = sentinel
	detailMetaStyle = sentinel
	detailSectionHeaderStyle = sentinel
	markdownCodeBlockStyle = sentinel
	titleBarStyle = sentinel
	statsLineStyle = sentinel
	tableHeaderStyle = sentinel
	separatorRuleStyle = sentinel
	cursorRowStyle = sentinel
	altRowStyle = sentinel
	normalRowStyle = sentinel
	footerBarStyle = sentinel
	modalBoxStyle = sentinel
	// Panel-border vars are TerminalColor (not Style); poison them
	// with the same sentinel value so we can detect a forgotten
	// rebuild via the same colorNone test.
	panelActiveBorder = sentinelColor
	panelInactiveBorder = sentinelColor

	applyColorMode(colorNone, false)

	all := []lipgloss.Style{
		titleStyle, subtleStyle, statusStyle, selectedStyle,
		openStyle, closedStyle, deletedStyle, helpKeyStyle,
		helpDescStyle, errorStyle, toastStyle, chipStyle,
		chipActive, tabActive, tabInactive,
		detailMetaStyle, detailSectionHeaderStyle, markdownCodeBlockStyle,
		titleBarStyle, statsLineStyle, tableHeaderStyle,
		separatorRuleStyle, cursorRowStyle, altRowStyle,
		normalRowStyle, footerBarStyle, modalBoxStyle,
	}
	for i, s := range all {
		if fg := s.GetForeground(); fg == sentinelColor {
			t.Fatalf("style %d not rebuilt by applyColorMode(colorNone): retained sentinel %v", i, fg)
		}
	}
	if panelActiveBorder == sentinelColor {
		t.Fatal("panelActiveBorder not rebuilt by applyColorMode(colorNone)")
	}
	if panelInactiveBorder == sentinelColor {
		t.Fatal("panelInactiveBorder not rebuilt by applyColorMode(colorNone)")
	}
}

// TestApplyColorMode_DeletedStyleIsRedFaint locks the M0 semantic
// remap: deletedStyle uses roborev's failStyle codes (124/196) with
// Faint so soft-deleted rows read as out-of-band but not alarming.
// Earlier the codes were gray (243/245) — that didn't differentiate
// from statusStyle.
func TestApplyColorMode_DeletedStyleIsRedFaint(t *testing.T) {
	applyColorMode(colorDark, true)
	assertStyleForeground(t, deletedStyle, "deletedStyle dark", "196")
	if !deletedStyle.GetFaint() {
		t.Fatal("deletedStyle must be faint so the red doesn't read as an error chip")
	}
}

func TestApplyColorMode_StatusColorsStayDistinctInWarmDisplays(t *testing.T) {
	applyColorMode(colorDark, true)
	assertStyleForeground(t, openStyle, "openStyle dark", "46")
	assertStyleForeground(t, closedStyle, "closedStyle dark", "245")

	applyColorMode(colorLight, false)
	assertStyleForeground(t, closedStyle, "closedStyle light", "240")
}

// TestApplyColorMode_PanelBorderColorsBound asserts the M3+ panel
// border vars are bound after a normal-mode apply. M0 introduces these
// vars even though the first usage lands in M3a — locking the values
// here keeps them honest.
func TestApplyColorMode_PanelBorderColorsBound(t *testing.T) {
	applyColorMode(colorDark, true)
	if panelActiveBorder == nil {
		t.Fatal("panelActiveBorder must be bound by applyColorMode(colorDark)")
	}
	if panelInactiveBorder == nil {
		t.Fatal("panelInactiveBorder must be bound by applyColorMode(colorDark)")
	}
	assertTerminalColor(t, panelActiveBorder, "panelActiveBorder dark", "205")
	assertTerminalColor(t, panelInactiveBorder, "panelInactiveBorder dark", "246")
}
