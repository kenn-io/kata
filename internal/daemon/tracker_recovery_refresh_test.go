package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

type retainedTrackerOwnersFixture struct {
	db.Storage
	checkpoints []db.TrackerOwnerCheckpoint
	history     map[string]db.TrackerExecutionFence
}

func (*retainedTrackerOwnersFixture) InstanceUID() string { return "01ARZ3NDEKTSV4RRFFQ69G5FAV" }
func (f *retainedTrackerOwnersFixture) ListTrackerOwnerCheckpoints(_ context.Context, root, project, after string, limit int) ([]db.TrackerOwnerCheckpoint, error) {
	out := []db.TrackerOwnerCheckpoint{}
	for _, cp := range f.checkpoints {
		if cp.RootInstanceUID == root && cp.ProjectUID == project && cp.OwnerInstanceUID > after {
			out = append(out, cp)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}
func (f *retainedTrackerOwnersFixture) LoadTrackerExecutionHistory(_ context.Context, root, project, role, write string) (db.TrackerExecutionFence, bool, error) {
	p, ok := f.history[write]
	return p, ok, nil
}

func (f *retainedTrackerOwnersFixture) ListTrackerRecoveryHistory(_ context.Context, root, project, after string, limit int) ([]db.TrackerExecutionFence, error) {
	keys := []string{}
	for key, p := range f.history {
		if p.RootInstanceUID == root && p.ProjectUID == project && key > after {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	out := []db.TrackerExecutionFence{}
	for _, key := range keys {
		out = append(out, f.history[key])
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// Contract: paging retained owners reaches accounts removed from current
// policy/binding, requires no provider resolver, and one unavailable old owner
// cannot prevent independent owners from receiving their original evidence.
func TestTrackerRecoveryVisitsEveryRetainedOwnerWithoutCurrentPolicy(t *testing.T) {
	f := &retainedTrackerOwnersFixture{history: map[string]db.TrackerExecutionFence{}}
	const project = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	previous := ""
	for i := 0; i < 129; i++ {
		selection := tracker.Selection{OwnerInstanceUID: workerSyntheticUID(100 + i), ConnectionUID: workerSyntheticUID(1000 + i), Generation: "retained"}
		head := db.TrackerExecutionFence{Version: 1, Role: "coordinator", RootInstanceUID: f.InstanceUID(), ProjectUID: project, Connection: selection, Sequence: uint64(i + 1), PreviousHead: previous, PendingEventUID: workerSyntheticUID(2000 + i), SourceKey: "notion:source", Locator: "item", Target: "closed", ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Phase: db.TrackerWriteAcknowledged, ProviderAcknowledged: true, Result: []byte(`{"status":"closed","version":"2030-01-01T00:00:00Z"}`)}
		var err error
		head.WriteUID, err = db.TrackerStatusWriteUID(head.RootInstanceUID, project, head.SourceKey, head.Locator, head.PendingEventUID, head.Target)
		require.NoError(t, err)
		head.Remote = &db.TrackerRemoteAttempt{UID: workerSyntheticUID(3000 + i), ArgumentsDigest: head.WriteUID, RootSequence: head.Sequence, RootPreviousHead: previous, OwnerSequence: 1, RootEpoch: head.ProcessEpoch, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB1"}
		owner, err := db.TrackerExecutorPreparationForCheckpoint(head)
		require.NoError(t, err)
		cp := db.TrackerOwnerCheckpoint{Version: 1, RootInstanceUID: f.InstanceUID(), ProjectUID: project, OwnerInstanceUID: selection.OwnerInstanceUID, Selection: selection, Sequence: 1, Head: db.TrackerFenceHead(owner), Attempt: *head.Remote, WriteUID: head.WriteUID, Disposition: db.TrackerWriteAcknowledged, ResultDigest: tracker.WorkerArgumentsDigest(head.Result)}
		require.NoError(t, cp.Validate())
		previous = db.TrackerFenceHead(head)
		f.checkpoints = append(f.checkpoints, cp)
		f.history[head.WriteUID] = head
	}
	// A first owner can have root-only non-send retirement but no settled
	// checkpoint when its preparation/terminal response was lost before rebind.
	var retired db.TrackerExecutionFence
	for _, proof := range f.history {
		if proof.Sequence == 129 {
			retired = proof
			break
		}
	}
	retired.Sequence++
	retired.PreviousHead = previous
	retired.Connection = tracker.Selection{OwnerInstanceUID: workerSyntheticUID(9999), ConnectionUID: workerSyntheticUID(10000), Generation: "retired-without-checkpoint"}
	retired.PendingEventUID = workerSyntheticUID(10001)
	retired.Phase = db.TrackerWriteNotSent
	retired.ProviderAcknowledged = false
	retired.Result = []byte(`{"not_sent":true}`)
	retired.Remote = &db.TrackerRemoteAttempt{UID: workerSyntheticUID(10002), ArgumentsDigest: retired.WriteUID, RootSequence: retired.Sequence, RootPreviousHead: previous, OwnerSequence: 1, RootEpoch: retired.ProcessEpoch, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB1"}
	var err error
	retired.WriteUID, err = db.TrackerStatusWriteUID(retired.RootInstanceUID, project, retired.SourceKey, retired.Locator, retired.PendingEventUID, retired.Target)
	require.NoError(t, err)
	require.NoError(t, retired.Validate())
	f.history[retired.WriteUID] = retired
	runtime := &TrackerRuntime{store: f} // no policy snapshot, clients or credentials
	visited := []tracker.Selection{}
	err = runtime.refreshRetainedTrackerOwners(t.Context(), project, func(_ context.Context, s tracker.Selection, provider string) error {
		visited = append(visited, s)
		require.Equal(t, "notion", provider)
		if len(visited) == 1 {
			return tracker.ErrUnavailable
		}
		return nil
	})
	require.ErrorIs(t, err, tracker.ErrUnavailable)
	require.Len(t, visited, 130)
	for i, cp := range f.checkpoints {
		require.Equal(t, cp.Selection, visited[i])
	}
	require.Equal(t, retired.Connection, visited[129], "first owner without checkpoint remains discoverable")
}
