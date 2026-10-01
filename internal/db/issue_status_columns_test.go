package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestIssueStatusColumnsPreserveNullObservation(t *testing.T) {
	stamp := "2026-09-29T14:00:00+02:00"
	pending := "01HZZZZZZZZZZZZZZZZZZZZZ13"
	locator := "7"
	absent, err := db.DecodeIssueStatusColumns(nil, nil, nil, nil)
	require.NoError(t, err)
	require.Nil(t, absent.Observed)
	observed, err := db.DecodeIssueStatusColumns(nil, &stamp, &pending, &locator)
	require.NoError(t, err)
	require.NotNil(t, observed.Observed)
	require.Nil(t, observed.Observed.Raw)
	require.Equal(t, time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), observed.Observed.Version)
	require.Equal(t, pending, observed.PendingEventUID)
	require.Equal(t, locator, observed.RemoteLocator)
	converted, err := db.NormalizeIssueStatusExport(db.ImportMappingExport{ObservedStatusAt: &stamp, PendingEventUID: &pending, RemoteLocator: &locator})
	require.NoError(t, err)
	require.Nil(t, converted.ObservedStatus)
	require.Equal(t, "2026-09-29T12:00:00Z", *converted.ObservedStatusAt)
}

func TestIssueStatusColumnsRejectIncompleteOrMalformedValues(t *testing.T) {
	raw, empty, badStamp, badUID := "open", "", "invalid", "bad-event"
	for _, input := range []struct{ raw, at, pending, locator *string }{
		{raw: &raw}, {at: &empty}, {at: &badStamp}, {pending: &badUID}, {pending: &empty}, {locator: &empty},
	} {
		_, err := db.DecodeIssueStatusColumns(input.raw, input.at, input.pending, input.locator)
		require.Error(t, err)
	}
}

func TestReplayPendingStatusRequiresExactMappedMutation(t *testing.T) {
	issueID := int64(1)
	issueUID := "01HZZZZZZZZZZZZZZZZZZZZZ11"
	pendingUID := "01HZZZZZZZZZZZZZZZZZZZZZ13"
	issue := &db.IssueExport{ID: issueID, UID: issueUID, ProjectID: 2}
	mapping := db.ImportMappingExport{ProjectID: 2, IssueID: &issueID, ObjectType: "issue", PendingEventUID: &pendingUID}
	event := db.EventExport{UID: pendingUID, ProjectID: 2, IssueID: &issueID, IssueUID: &issueUID, Type: "issue.closed"}
	require.NoError(t, db.ValidateImportRecords([]db.ImportRecord{issue, &mapping, &event}))
	for _, name := range []string{"missing", "wrong issue", "wrong project", "wrong mutation", "non-issue mapping"} {
		t.Run(name, func(t *testing.T) {
			invalidEvent, invalidMapping := event, mapping
			records := []db.ImportRecord{issue, &invalidMapping, &invalidEvent}
			switch name {
			case "missing":
				records = records[:2]
			case "wrong issue":
				other := "01HZZZZZZZZZZZZZZZZZZZZZ12"
				invalidEvent.IssueUID = &other
			case "wrong project":
				invalidEvent.ProjectID = 3
			case "wrong mutation":
				invalidEvent.Type = "issue.updated"
			case "non-issue mapping":
				invalidMapping.ObjectType = "comment"
			}
			require.Error(t, db.ValidateImportRecords(records))
		})
	}
}
