package dbtest

import (
	"archive/zip"
	"encoding/json/jsontext"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// RunRecurrenceAttachmentKeepsDeadlineZone checks that attaching a recurrence preserves the deadline zone.
func RunRecurrenceAttachmentKeepsDeadlineZone(t *testing.T, open func(*testing.T) db.Storage) {
	for _, tc := range []struct {
		name                 string
		issueZone            jsontext.Value
		defaultZone          string
		deadline             string
		wantZone             string
		local, localMismatch bool
	}{
		{"daemon zone", nil, "America/New_York", `"2026-05-19T09:00"`, "America/New_York", false, false},
		{"legacy Local instant", jsontext.Value(`"Local"`), "UTC", `"2026-05-19T09:00:00Z"`, "America/New_York", true, false},
		{"legacy Local civil mismatched rules", nil, "Local", `"2026-11-12T09:00"`, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.local {
				previous := time.Local
				location, err := metadata.ScheduleLocation("America/New_York", metadata.ScheduleTZDataVersion)
				require.NoError(t, err)
				time.Local = location
				if tc.localMismatch {
					archive, err := zip.OpenReader("../../metadata/testdata/zoneinfo-2026e-inuvik.zip")
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, archive.Close()) })
					data, err := fs.ReadFile(archive, "America/Inuvik")
					require.NoError(t, err)
					time.Local, err = time.LoadLocationFromTZData("America/Inuvik", data)
					require.NoError(t, err)
				}
				t.Cleanup(func() { time.Local = previous })
			}
			store := open(t)
			ctx := t.Context()
			project := createCursorProject(ctx, t, store)
			fields := map[string]jsontext.Value{"deadline_on": jsontext.Value(tc.deadline)}
			if tc.issueZone != nil {
				fields["timezone"] = tc.issueZone
			}
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Example", Author: "user-a", Metadata: fields})
			require.NoError(t, err)
			_, _, before, _, err := metadata.ScheduleFieldInstant(string(issue.Metadata), "deadline_on", tc.defaultZone)
			require.NoError(t, err)
			_, err = store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: issue.ID, DefaultTimezone: tc.defaultZone, Recurrence: db.CreateRecurrenceIn{ProjectID: project.ID, Actor: "user-a", Rule: "FREQ=DAILY", DTStart: "2026-05-18", Timezone: "Europe/London", Template: db.RecurrenceTemplate{Title: "Recurring"}}})
			if tc.localMismatch {
				require.ErrorContains(t, err, "Local timezone rules differ")
				saved, readErr := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, readErr)
				require.Equal(t, issue.Metadata, saved.Metadata)
				require.Nil(t, saved.RecurrenceID)
				return
			}
			require.NoError(t, err)
			saved, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, zone, after, _, err := metadata.ScheduleFieldInstant(string(saved.Metadata), "deadline_on", tc.defaultZone)
			require.NoError(t, err)
			require.Equal(t, tc.wantZone, zone)
			require.Equal(t, before, after)
		})
	}
}
