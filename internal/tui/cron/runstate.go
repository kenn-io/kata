package cron

import (
	"charm.land/lipgloss/v2"
	"go.kenn.io/kata/internal/tui/cron/records"
)

// Keep the source role palette while displaying the native status verbatim.
func runLabel(r records.Run) (string, lipgloss.Style) {
	switch r.Status {
	case "running":
		return r.Status, openStyle
	case "succeeded", "cancelled":
		return r.Status, closedStyle
	case "failed":
		return r.Status, errorStyle
	default:
		return r.Status, attentionStyle
	}
}
