package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewrelay"
)

func TestCloseFingerprintBindsAgentsViewAssociation(t *testing.T) {
	legacy := closeIdempotencyFingerprint("issue", "ref", "actor", "done", "message", "", nil, false, nil, nil)
	require.Equal(t, legacy, closeIdempotencyFingerprint("issue", "ref", "actor", "done", "message", "", nil, false, nil, nil, nil))
	a := &agentsviewrelay.Association{SnapshotSHA256: strings.Repeat("a", 64)}
	first := closeIdempotencyFingerprint("issue", "ref", "actor", "done", "message", "", nil, false, nil, nil, a)
	a.SnapshotSHA256 = strings.Repeat("b", 64)
	require.NotEqual(t, first, closeIdempotencyFingerprint("issue", "ref", "actor", "done", "message", "", nil, false, nil, nil, a))
}
