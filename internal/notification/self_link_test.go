package notification

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

// Contract: self-links do not notify, including confirmation fan-out.
func TestSelfConfirmDoesNotFanOut(t *testing.T) {
	identity := Identity{Actor: "worker", Teammate: "builder"}
	patch, err := LinkPatch(LinkInput{Sender: identity, Target: identity, Kind: "confirm", ReplyUID: "new", TargetUID: "original", Owner: "owner", ParentOwner: "coordinator", PriorLinkers: []Identity{{Actor: "reviewer"}}})
	require.NoError(t, err)
	require.Empty(t, patch)
}

func TestSelfReplyClearsMatchingRequest(t *testing.T) {
	identity := Identity{Actor: "worker", Teammate: "builder"}
	key := MetadataKey("worker/builder")
	patch, err := LinkPatch(LinkInput{Sender: identity, Target: identity, Kind: "reply", ReplyUID: "new", TargetUID: "original", Current: map[string]jsontext.Value{key: jsontext.Value(`{"from":"lead","message":"respond","re":"original"}`)}})
	require.NoError(t, err)
	require.Equal(t, map[string]jsontext.Value{key: jsontext.Value("null")}, patch)
}
