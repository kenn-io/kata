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

// Answering your own comment still settles a request about it.
func TestSelfReplyClearsMatchingRequest(t *testing.T) {
	identity := Identity{Actor: "worker", Teammate: "builder"}
	key := MetadataKey("worker/builder")
	slots := map[string]jsontext.Value{key: jsontext.Value(`{"from":"lead","message":"respond","re":"original"}`)}
	patch, err := LinkPatch(LinkInput{Sender: identity, Target: identity, Kind: "reply", ReplyUID: "new", TargetUID: "original", Current: slots})
	require.NoError(t, err)
	require.Empty(t, patch)
	link, err := LinkValue(identity, "new", "reply")
	require.NoError(t, err)
	require.Equal(t, map[string]jsontext.Value{key: jsontext.Value("null"), MetadataKey("lead"): link},
		SettlePatch(slots, identity, "original", link))
}
