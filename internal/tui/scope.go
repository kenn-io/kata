package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// renderEmpty draws the centered onboarding hint shown when the daemon
// has zero registered projects. lipgloss.Place handles vertical and
// horizontal centering inside width × height; small terminals fall back
// to top-left placement (lipgloss caps the offsets) so the message
// remains visible.
func renderEmpty(width, height int) string {
	body := strings.Join([]string{
		titleStyle.Render("no kata projects registered yet"),
		"",
		subtleStyle.Render("run `kata init` in a repo to get started."),
		subtleStyle.Render("press q to quit."),
	}, "\n")
	if width <= 0 || height <= 0 {
		return body
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, body)
}

// renderTooNarrow is the degraded full-screen hint shown when the
// terminal width can't fit readable table columns + chip strip.
//
// q / ctrl+c still route through Model.routeGlobalKey, so the user
// can quit from the hint screen without resizing first.
func renderTooNarrow(width, height int) string {
	msg := strings.Join([]string{
		"kata tui needs more space",
		"",
		">=80 columns wide",
		"resize your terminal and try again",
		"",
		"press q to quit",
	}, "\n")
	body := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(panelActiveBorder).
		Padding(1, 2).
		Render(msg)
	if width <= 0 || height <= 0 {
		return body
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, body)
}
