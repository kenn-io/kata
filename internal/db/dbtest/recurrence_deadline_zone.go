package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// RunRecurrenceAttachmentKeepsDeadlineZone checks that attaching a recurrence preserves the deadline zone.
func RunRecurrenceAttachmentKeepsDeadlineZone(t *testing.T, open func(*testing.T) db.Storage) {
	for _, tc := range []struct {
		name        string
		issueZone   jsontext.Value
		defaultZone string
	}{
		{"issue zone", jsontext.Value(`"America/New_York"`), "UTC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := open(t)
			ctx := t.Context()
			project := createCursorProject(ctx, t, store)
			fields := map[string]jsontext.Value{"deadline_on": jsontext.Value(`"2026-05-19T09:00"`)}
			if tc.issueZone != nil {
				fields["timezone"] = tc.issueZone
			}
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Example", Author: "user-a", Metadata: fields})
			require.NoError(t, err)
			_, _, before, _, err := metadata.ScheduleFieldInstant(string(issue.Metadata), "deadline_on", tc.defaultZone)
			require.NoError(t, err)
			_, err = store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{IssueID: issue.ID, DefaultTimezone: tc.defaultZone, Recurrence: db.CreateRecurrenceIn{ProjectID: project.ID, Actor: "user-a", Rule: "FREQ=DAILY", DTStart: "2026-05-18", Timezone: "Europe/London", Template: db.RecurrenceTemplate{Title: "Recurring"}}})
			require.NoError(t, err)
			saved, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, zone, after, _, err := metadata.ScheduleFieldInstant(string(saved.Metadata), "deadline_on", tc.defaultZone)
			require.NoError(t, err)
			require.Equal(t, "America/New_York", zone)
			require.Equal(t, before, after)
		})
	}
}
