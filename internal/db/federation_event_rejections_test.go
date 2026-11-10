package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRejectedCreationDefersCronRun(t *testing.T) {
	rejected := FederationRejectedCreations{"issue-1": {IssueUID: "issue-1"}}
	for _, kind := range []string{"cron.run.observed", "cron.run.snapshot"} {
		t.Run(kind, func(t *testing.T) {
			event := RemoteEvent{Type: kind, Payload: []byte(`{"uid":"run-1","issue_uid":"issue-1"}`)}
			rejection, dependent, err := rejected.Dependent(event)
			require.NoError(t, err)
			require.True(t, dependent)
			require.Equal(t, "issue-1", rejection.IssueUID)
		})
	}
}
