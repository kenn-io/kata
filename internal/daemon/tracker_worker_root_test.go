package daemon

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/todoistsync"
	"testing"
)

func TestTrackerWriteArgumentsKeepNativeImmutableMapping(t *testing.T) {
	require.Equal(t, "page:item", workerArgumentsExternalID("notion", trackerWorkerArguments{ID: "item"}))
	require.Equal(t, "issue:item", workerArgumentsExternalID("linear", trackerWorkerArguments{ID: "item"}))
	require.Equal(t, "work-item:item", workerArgumentsExternalID("plane", trackerWorkerArguments{ID: "item"}))
	require.Equal(t, "task:item", workerArgumentsExternalID("ticktick", trackerWorkerArguments{ID: "item"}))
	require.Equal(t, "task:item", workerArgumentsExternalID("twenty", trackerWorkerArguments{ID: "item"}))
	require.Equal(t, "task:item", workerArgumentsExternalID("todoist", trackerWorkerArguments{TodoistTarget: &todoistsync.StatusTarget{ID: "item"}}))
	require.Equal(t, "node", workerArgumentsExternalID("github", trackerWorkerArguments{ID: "node"}))
	require.Empty(t, workerArgumentsExternalID("todoist", trackerWorkerArguments{ID: "item"}), "a different argument shape cannot replace the exact target")
}

// A canceled transport must interrupt the serialized original-intent read even
// while the longer status pass remains active; no preparation may be admitted.
func TestTrackerRootStageAdmissionUsesTransportCancellation(t *testing.T) {
	f := newTrackerCoordinatorFixture(t)
	snapshot, err := f.runtime.current()
	require.NoError(t, err)
	entered := make(chan context.Context, 1)
	ticket := trackerWorkerTicketFixtureForResult()
	ticket.RootEpoch = snapshot.incarnation
	state := &trackerRootWorkerWrite{ctx: f.ctx, runtime: f.runtime, ticket: ticket, originalAdmit: func(ctx context.Context) error { entered <- ctx; <-ctx.Done(); return ctx.Err() }}
	transport, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := state.stage(transport, trackerWorkerSignal{CallUID: ticket.CallUID, SessionUID: ticket.SessionUID, Stage: "prepare"})
		done <- err
	}()
	read := <-entered
	require.NoError(t, read.Err())
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, f.ctx.Err())
	require.Nil(t, state.prepared)
	require.Nil(t, state.dispatched)
}
