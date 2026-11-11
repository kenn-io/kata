package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/metadata"
)

func TestDatesFormRejectsUnknownScheduleRules(t *testing.T) {
	for _, tc := range []struct {
		name, raw, wantError string
		sibling              bool
	}{
		{"unknown rules", `{"value":"2026-11-12","timezone":"UTC","tzdata_version":"unknown"}`, "unsupported schedule tzdata version", false},
		{"unknown members", `{"value":"2026-11-12","timezone":"UTC","tzdata_version":"` + metadata.ScheduleTZDataVersion + `","future":true}`, "unknown object member name", false},
		{"replace opaque date", `{"legacy":"opaque"}`, "", false},
		{"edit healthy sibling", `{"legacy":"opaque"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDatesHarness(t, nil, viewDetail)
			if tc.wantError != "" {
				// Unsupported rows can still arrive from an older daemon or cache.
				h.m.detail.issue.Metadata.ScheduledOn = jsontext.Value(tc.raw)
				h.m.list.issues = []Issue{*h.m.detail.issue}
			} else {
				records, err := dbtest.CollectImportRecords(t.Context(), h.env.DB, db.ExportFilter{ProjectID: &h.issue.ProjectID})
				require.NoError(t, err)
				for _, record := range records {
					if issue, ok := record.(*db.IssueExport); ok && issue.UID == h.issue.UID {
						issue.Metadata = jsontext.Value(`{"scheduled_on":` + tc.raw + `,"deadline_on":"2026-11-02"}`)
					}
				}
				require.NoError(t, h.env.DB.ImportReplay(t.Context(), records, db.ImportOptions{}))
				h.reload()
			}
			h.openForm()
			before := h.revision()
			if tc.sibling {
				h.fill("", "2026-12-02")
			} else {
				h.fill("2026-12-01", h.m.input.dates.deadline)
			}
			h.save()
			if tc.wantError != "" {
				require.Contains(t, h.m.input.err, tc.wantError)
				require.Equal(t, before, h.revision())
				return
			}
			require.Empty(t, h.m.input.err)
			require.Equal(t, inputNone, h.m.input.kind)
			require.Greater(t, h.revision(), before)
			current, err := h.env.DB.IssueByID(t.Context(), h.issue.ID)
			require.NoError(t, err)
			var fields map[string]jsontext.Value
			require.NoError(t, json.Unmarshal([]byte(current.Metadata), &fields))
			if tc.sibling {
				require.JSONEq(t, tc.raw, string(fields["scheduled_on"]))
				require.Equal(t, "2026-12-02", metadata.ScheduleClockText(fields["deadline_on"]))
			} else {
				require.Equal(t, "2026-12-01", metadata.ScheduleClockText(fields["scheduled_on"]))
			}
		})
	}
}
