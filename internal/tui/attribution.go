package tui

import "go.kenn.io/kata/internal/db"

// The daemon supplies verified creation projections. Source labels remain
// visible for pending and legacy records without asserting accountability.
func creationAttributionLines(view db.AttributionView, author, teammate string, width int) []string {
	if view.Verification == "" {
		return nil
	}
	state := view.Verification
	if state != "verified" && state != "pending" {
		state = "legacy"
	}
	fields := []string{"creation: " + state}
	if state == "verified" && view.AccountableActor != "" {
		fields = append(fields, "accountable: "+sanitizeForLine(view.AccountableActor))
	}
	source := view.SourceActor
	if source == "" {
		source = author
	}
	if teammate != "" {
		source += " / " + teammate
	}
	fields = append(fields, "source: "+sanitizeForLine(source))
	var lines []string
	for _, field := range fields {
		lines = append(lines, wrapBody(field, width)...)
	}
	return lines
}
