package daemon

import (
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewrelay"
)

// The request contract must fail closed before an ordinary close when the
// discovery verifier is not integrated; missing proof never becomes consent.
func TestAgentsViewAssociationRequiresDiscoveryAdapter(t *testing.T) {
	_, err := decodeAgentsViewAssociation(jsontext.Value(`{"v":1}`))
	require.Error(t, err)
	_, err = decodeAgentsViewAssociation(jsontext.Value(`{"v":1,"v":1}`))
	require.Error(t, err)
}

func TestCloseFingerprintBindsAgentsViewAssociation(t *testing.T) {
	legacy := closeIdempotencyFingerprint("issue", "ref", "actor", "done", "message", "", nil, false, nil, nil)
	require.Equal(t, legacy, closeIdempotencyFingerprint("issue", "ref", "actor", "done", "message", "", nil, false, nil, nil, nil))
	a := &agentsviewrelay.Association{SnapshotSHA256: strings.Repeat("a", 64)}
	first := closeIdempotencyFingerprint("issue", "ref", "actor", "done", "message", "", nil, false, nil, nil, a)
	a.SnapshotSHA256 = strings.Repeat("b", 64)
	require.NotEqual(t, first, closeIdempotencyFingerprint("issue", "ref", "actor", "done", "message", "", nil, false, nil, nil, a))
}
