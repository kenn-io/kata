package jsonl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOrphanReportDropOutranksScrub pins drop precedence in BOTH arrival
// orders. PRAGMA foreign_key_check emits one row per violated FK, so an
// events row with both issue_id and related_issue_id orphaned is classified
// twice; which classification arrives first is an implementation detail of
// SQLite's scan order, and the report must not depend on it.
func TestOrphanReportDropOutranksScrub(t *testing.T) {
	row := orphanRow{Table: "events", RowID: 7}

	t.Run("drop then scrub", func(t *testing.T) {
		report := newOrphanReport()
		report.record(row, dispositionDrop)
		report.record(row, dispositionScrub)
		assert.Equal(t, 1, report.DropCount("events"))
		assert.Equal(t, 0, report.ScrubCount("events"))
	})

	t.Run("scrub then drop", func(t *testing.T) {
		report := newOrphanReport()
		report.record(row, dispositionScrub)
		report.record(row, dispositionDrop)
		assert.Equal(t, 1, report.DropCount("events"),
			"drop must win no matter which violation is observed first")
		assert.Equal(t, 0, report.ScrubCount("events"))
	})

	t.Run("same disposition twice counts once", func(t *testing.T) {
		report := newOrphanReport()
		report.record(row, dispositionScrub)
		report.record(row, dispositionScrub)
		assert.Equal(t, 1, report.ScrubCount("events"))
	})

	t.Run("counts are per table", func(t *testing.T) {
		report := newOrphanReport()
		report.record(orphanRow{Table: "events", RowID: 1}, dispositionDrop)
		report.record(orphanRow{Table: "comments", RowID: 1}, dispositionDrop)
		assert.Equal(t, 1, report.DropCount("events"))
		assert.Equal(t, 1, report.DropCount("comments"))
		assert.Equal(t, 0, report.DropCount("links"))
	})
}
