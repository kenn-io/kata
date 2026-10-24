// Source: go.kenn.io/kata f3a06c1 internal/tui/theme.go:119-236,
// internal/colormode/colormode.go:26-40. Change only by re-copying.

package cron

import "strings"

type colorMode int

const (
	colorAuto colorMode = iota
	colorDark
	colorLight
	colorNone
)

func resolveColorMode(getenv func(string) string) colorMode {
	if getenv("NO_COLOR") != "" {
		return colorNone
	}
	switch strings.ToLower(getenv("KATA_COLOR_MODE")) {
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
