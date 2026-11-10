package issuesync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Explicit activation/recovery can observe every pending item without granting
// permission to send it or clearing a newer/local intent by convergence alone.
func TestReadOnlyReconciliationReadsPendingWithoutDispatchOrAcknowledgement(t *testing.T) {
	store, binding, mappings, at := statusFixture(t, 1)
	_, event, _, err := store.CloseIssue(t.Context(), *mappings[0].Mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	reads, writes := 0, 0
	run := &statusTestRun{read: func(context.Context, db.IssueStatusMapping) (StatusObservation, error) {
		reads++
		return observed("open", at.Add(time.Second)), nil
	}, write: func(context.Context, db.IssueStatusMapping, string, func() error) (StatusObservation, error) {
		writes++
		return observed("closed", at), nil
	}}
	runner := NewRunner(RunnerConfig{Store: store, Adapter: statusAdapter(binding, run), Clock: func() time.Time { return at }})
	_, err = runner.RunOnce(WithReadOnlyStatusReconciliation(t.Context()), binding.ID)
	require.NoError(t, err)
	require.Positive(t, reads)
	require.Zero(t, writes)
	checkpoint := privateMapping(t, store, mappings[0])
	require.NotNil(t, checkpoint.PendingEventUID)
	require.Equal(t, event.UID, *checkpoint.PendingEventUID)
	require.NotNil(t, checkpoint.ObservedStatus)
	require.Equal(t, "open", *checkpoint.ObservedStatus)
	issue, err := store.IssueByID(t.Context(), *mappings[0].Mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "closed", issue.Status)
}
