package client

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client/generated"
)

// A changes block whose edit touched no parent carries no parent_set or
// parent_removed peer; the generated client must validate and round-trip it
// without inventing empty peer objects.
func TestLinkChangesWithoutParentPeersValidatesAndOmitsParentPeers(t *testing.T) {
	changes := generated.LinkChanges{}
	require.NoError(t, changes.Validate(),
		"a changes block without parent peers must validate")

	out, err := json.Marshal(changes)
	require.NoError(t, err)
	var roundTrip map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &roundTrip))
	require.NotContains(t, roundTrip, "parent_set",
		"absent parent_set must not marshal as an empty object")
	require.NotContains(t, roundTrip, "parent_removed",
		"absent parent_removed must not marshal as an empty object")
}
