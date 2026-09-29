package db_test

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestIssueStatusStateValidation(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{"observed":{}}`, `{"observed":{"version":"2026-09-29T12:00:00Z"}}`,
		`{"observed":{"raw":null}}`, `{"observed":{"raw":null,"version":"invalid"}}`,
		`{"observed":{"raw":2,"version":"2026-09-29T12:00:00Z"}}`,
		`{"pending_event_uid":"not-an-event"}`, `{"github_issue_number":-1}`, `{"github_issue_number":0}`,
		`{"github_issue_number":1.5}`, `{"desired_state":"closed"}`, `{"observed":null}`,
	} {
		_, err := db.DecodeIssueStatusState(jsontext.Value(raw))
		require.Error(t, err, raw)
	}
	absent, err := db.DecodeIssueStatusState(nil)
	require.NoError(t, err)
	require.Nil(t, absent.Observed)
	empty, err := db.DecodeIssueStatusState(jsontext.Value(`{}`))
	require.NoError(t, err)
	require.Nil(t, empty.Observed)
	observed, err := db.DecodeIssueStatusState(jsontext.Value(`{"observed":{"raw":null,"version":"2026-09-29T14:00:00+02:00"},"github_issue_number":7,"pending_event_uid":"01HZZZZZZZZZZZZZZZZZZZZZ13"}`))
	require.NoError(t, err)
	require.NotNil(t, observed.Observed)
	require.Nil(t, observed.Observed.Raw)
	require.Equal(t, time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), observed.Observed.Version)
	require.Equal(t, "7", observed.RemoteLocator)
	require.Equal(t, "01HZZZZZZZZZZZZZZZZZZZZZ13", observed.PendingEventUID)
}
