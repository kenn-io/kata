// Package colormode resolves kata's terminal palette preference.
package colormode

import (
	"os"
	"strings"
)

// Mode describes whether terminal colors should target a light or dark
// background. Auto leaves background brightness unknown until a caller can
// detect it, while None disables color-specific surfaces.
type Mode int

const (
	// Auto leaves background brightness for the caller to detect.
	Auto Mode = iota
	// Dark targets a terminal with a dark background.
	Dark
	// Light targets a terminal with a light background.
	Light
	// None disables color-specific surfaces.
	None
)

// Resolve honors NO_COLOR over KATA_COLOR_MODE. Unrecognized values use Auto.
func Resolve() Mode {
	if os.Getenv("NO_COLOR") != "" {
		return None
	}
	switch strings.ToLower(os.Getenv("KATA_COLOR_MODE")) {
	case "dark":
		return Dark
	case "light":
		return Light
	case "none":
		return None
	default:
		return Auto
	}
}
