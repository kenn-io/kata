package dbtest

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
)

// RunRecurrenceAttachmentKeepsDeadlineZone checks that attaching a recurrence preserves the deadline zone.
func RunRecurrenceAttachmentKeepsDeadlineZone(t *testing.T, open func(*testing.T) db.Storage) {
	for _, tc := range []struct {
		name          string
		issueZone     jsontext.Value
		defaultZone   string
		deadline      string
		wantZone      string
		localMismatch bool
	}{
		{"issue zone", jsontext.Value(`"America/New_York"`), "UTC", `"2026-05-19T09:00"`, "America/New_York", false},
		{"daemon zone", nil, "America/New_York", `"2026-05-19T09:00"`, "America/New_York", false},
		{"legacy Local instant", jsontext.Value(`"Local"`), "UTC", `"2026-05-19T09:00:00Z"`, "America/New_York", false},
		{"legacy Local civil", jsontext.Value(`"Local"`), "Local", `"2026-05-19T09:00"`, "America/New_York", false},
		{"legacy Local civil mismatched rules", jsontext.Value(`"Local"`), "Local", `"2026-05-19T09:00"`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.defaultZone == "Local" || tc.name == "legacy Local instant" {
				t.Setenv("TZ", "America/New_York")
				previous := time.Local
				localZone := recurrenceclient.LocalZone("/etc/localtime")
				location, err := time.LoadLocation(localZone)
				require.NoError(t, err)
				time.Local = location
				tc.wantZone = localZone
				if tc.localMismatch {
					_, offset := time.Date(2026, 5, 19, 9, 0, 0, 0, location).Zone()
					time.Local = time.FixedZone(localZone, offset+3600)
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
