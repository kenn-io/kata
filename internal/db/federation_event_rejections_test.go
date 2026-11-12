package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRejectedCreationDefersCronRun(t *testing.T) {
	const issueUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	rejected := FederationRejectedCreations{issueUID: {IssueUID: issueUID}}
	for _, kind := range []string{"cron.run.observed", "cron.run.snapshot"} {
		t.Run(kind, func(t *testing.T) {
			event := RemoteEvent{Type: kind, Payload: []byte(`{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`)}
			rejection, dependent, err := rejected.Dependent(event)
			require.NoError(t, err)
			require.True(t, dependent)
			require.Equal(t, issueUID, rejection.IssueUID)
		})
	}
}
