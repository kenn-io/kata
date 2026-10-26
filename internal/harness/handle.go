package harness

import (
	"fmt"
	"regexp"
	"strings"
)

// Mode selects the detail included in inferred teammate attribution.
type Mode string

const (
	// ModeModel includes detected model detail in inferred teammate attribution.
	ModeModel Mode = "model"
	// ModeHarness uses only the detected harness for inferred attribution.
	ModeHarness Mode = "harness"
	// ModeOff disables inferred teammate attribution.
	ModeOff Mode = "off"
)

// ParseMode validates the configured inference mode.
func ParseMode(s string) (Mode, error) {
	switch mode := Mode(s); mode {
	case ModeModel, ModeHarness, ModeOff:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid teammate inference mode %q: must be model, harness or off", s)
	}
}

// Handle builds teammate attribution from a detected canonical harness.
// Off mode or an absent harness produces no attribution.
func Handle(d Detection, mode Mode) string {
	if mode == ModeOff || d.Harness == "" {
		return ""
	}
	if mode == ModeHarness {
		return d.Harness
	}
	if part := modelPart(d); part != "" {
		return d.Harness + "." + part
	}
	return d.Harness
}

var modelSlugPattern = regexp.MustCompile(`[^a-z0-9._-]+`)

// modelPart retains model identity in the space available after the canonical
// harness and dot. Keep the slug rules here shared with model producers.
func modelPart(d Detection) string {
	model := strings.ToLower(d.Model)
	// A provider alone cannot turn an unknown or unusable model into attribution.
	if strings.Trim(modelSlugPattern.ReplaceAllString(model, "-"), "-_.") == "" {
		return ""
	}
	provider := strings.ToLower(d.Provider)
	if provider != "" && !strings.HasPrefix(model, provider) {
		model = provider + "-" + model
	}
	model = strings.TrimPrefix(model, d.Harness+"-")
	part := strings.Trim(modelSlugPattern.ReplaceAllString(model, "-"), "-_.")
	return part[:min(len(part), 64-len(d.Harness)-1)]
}
