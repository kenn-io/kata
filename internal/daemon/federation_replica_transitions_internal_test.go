package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestFederationReplicaRegistry() *federationReplicaRegistry {
	return &federationReplicaRegistry{
		transitions: make(map[string]*federationReplicaTransition),
	}
}

func TestFederationReplicaRegistryStateTransitions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(r *federationReplicaRegistry, key string)
		want  federationReplicaTransitionState
	}{
		{
			name:  "unseen key is idle",
			apply: func(*federationReplicaRegistry, string) {},
			want:  federationReplicaIdle,
		},
		{
			name: "prepared leave is leave-pending",
			apply: func(r *federationReplicaRegistry, key string) {
				r.markLeavePending(key)
			},
			want: federationReplicaLeavePending,
		},
		{
			name: "completed leave is left",
			apply: func(r *federationReplicaRegistry, key string) {
				r.markLeavePending(key)
				r.markLeft(key)
			},
			want: federationReplicaLeft,
		},
		{
			name: "leave completed without preparation is left",
			apply: func(r *federationReplicaRegistry, key string) {
				r.markLeft(key)
			},
			want: federationReplicaLeft,
		},
		{
			name: "rolled back preparation is idle again",
			apply: func(r *federationReplicaRegistry, key string) {
				r.markLeavePending(key)
				r.clearLeave(key)
			},
			want: federationReplicaIdle,
		},
		{
			name: "successful ensure clears a completed leave",
			apply: func(r *federationReplicaRegistry, key string) {
				r.markLeft(key)
				r.clearLeave(key)
			},
			want: federationReplicaIdle,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := newTestFederationReplicaRegistry()
			tc.apply(registry, "key")
			assert.Equal(t, tc.want, registry.state("key"))
		})
	}
}

func TestFederationReplicaRegistryForgetsIdleKeys(t *testing.T) {
	registry := newTestFederationReplicaRegistry()
	registry.markLeavePending("key")
	registry.clearLeave("key")
	assert.Empty(t, registry.transitions,
		"an idle key with nothing in flight must not retain a record")

	finish := registry.registerHubOperationLocked("key")
	registry.markLeft("key")
	finish()
	assert.Equal(t, federationReplicaLeft, registry.state("key"),
		"draining the last operation must not discard leave state")
}

func TestFederationReplicaRegistryBlocksEntryPerLeaveState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		apply   func(r *federationReplicaRegistry, key string)
		message string
	}{
		{
			name:  "idle admits new hub operations",
			apply: func(*federationReplicaRegistry, string) {},
		},
		{
			name: "prepared leave blocks with the pending message",
			apply: func(r *federationReplicaRegistry, key string) {
				r.markLeavePending(key)
			},
			message: "explicit federation leave is pending",
		},
		{
			name: "completed leave blocks with the left message",
			apply: func(r *federationReplicaRegistry, key string) {
				r.markLeft(key)
			},
			message: "federation mapping was explicitly left",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := newTestFederationReplicaRegistry()
			tc.apply(registry, "key")
			err := registry.leaveBlockedError("key")
			if tc.message == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrFederationReplicaLeavePending)
			assert.EqualError(t, err, tc.message)
		})
	}
}

func TestFederationReplicaRegistryDrainSignalTracksInFlightOperations(t *testing.T) {
	registry := newTestFederationReplicaRegistry()
	_, waiting := registry.drainSignal("key")
	assert.False(t, waiting, "nothing in flight must not require a wait")

	first := registry.registerHubOperationLocked("key")
	second := registry.registerHubOperationLocked("key")
	drained, waiting := registry.drainSignal("key")
	require.True(t, waiting)

	first()
	first()
	select {
	case <-drained:
		require.FailNow(t, "drain closed while an operation was still in flight")
	default:
	}

	second()
	select {
	case <-drained:
	default:
		require.FailNow(t, "drain did not close after the last operation finished")
	}

	_, waiting = registry.drainSignal("key")
	assert.False(t, waiting, "a fully drained key must not require a wait")

	registry.registerHubOperationLocked("key")
	reopened, waiting := registry.drainSignal("key")
	require.True(t, waiting)
	select {
	case <-reopened:
		require.FailNow(t, "a newly registered operation reused the closed drain channel")
	default:
	}
}
