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

func FuzzSelfLinkNeverNotifies(f *testing.F) {
	f.Add("owner", "coordinator", "reviewer")
	f.Fuzz(func(t *testing.T, owner, parent, linker string) {
		if len(owner)+len(parent)+len(linker) > 128 {
			return
		}
		identity := Identity{Actor: "worker", Teammate: "builder"}
		patch, err := LinkPatch(LinkInput{Sender: identity, Target: identity, Kind: "confirm", ReplyUID: "new", TargetUID: "original", Owner: owner, ParentOwner: parent, PriorLinkers: []Identity{{Actor: linker}}})
		require.NoError(t, err)
		require.Empty(t, patch, "a self-confirmation must not fan out to other recipients")
	})
}

func TestSelfReplyClearsMatchingRequest(t *testing.T) {
	identity := Identity{Actor: "worker", Teammate: "builder"}
	key := MetadataKey("worker/builder")
	patch, err := LinkPatch(LinkInput{Sender: identity, Target: identity, Kind: "reply", ReplyUID: "new", TargetUID: "original", Current: map[string]jsontext.Value{key: jsontext.Value(`{"from":"lead","message":"respond","re":"original"}`)}})
	require.NoError(t, err)
	require.Equal(t, map[string]jsontext.Value{key: jsontext.Value("null")}, patch)
}
