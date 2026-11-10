package tracker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Contract: reverse worker identity comes from live negotiated enrollment, not
// from a body-selected owner, same-name actor, or an ordinary member assertion.
func TestWorkerPresencePreservesOwnerThroughTrustedRelay(t *testing.T) {
	const owner = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const relay = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	const root = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	const project = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	const binding = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
	p := WorkerPresence{Version: 1, RootInstanceUID: root, ProjectUID: project, OwnerInstanceUID: owner, OwnerEpoch: binding, ExpiresAt: time.Unix(1900000120, 0).Unix(), Path: []RelayHop{{InstanceUID: owner, BindingUID: binding, ResetEpoch: 1}, {InstanceUID: relay, BindingUID: binding, ResetEpoch: 2}}}
	now := time.Unix(1900000000, 0)
	require.NoError(t, p.Validate(root, relay, binding, 2, true, now))
	require.Error(t, p.Validate(root, relay, binding, 2, false, now))
	changed := p
	changed.OwnerInstanceUID = relay
	require.Error(t, changed.Validate(root, relay, binding, 2, true, now), "cannot substitute a relay's owner identity")
	require.Error(t, p.Validate(root, relay, binding, 3, true, now), "reset epoch fences stale enrollment")
	require.Error(t, p.Validate(root, relay, binding, 2, true, now.Add(121*time.Second)))
}
