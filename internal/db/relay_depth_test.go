package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRelayAuthorityDepthIncludesFinalLeaf(t *testing.T) {
	root := "00000000000000000000000001"
	leaf := "00000000000000000000000009"
	c := db.RelayBindingConfig{ProtocolVersion: 1, BindingUID: "00000000000000000000000012", UpstreamInstanceUID: "00000000000000000000000008", AuthorityUID: root, HubPath: []string{root, "00000000000000000000000002", "00000000000000000000000003", "00000000000000000000000004", "00000000000000000000000005", "00000000000000000000000006", "00000000000000000000000007", "00000000000000000000000008", leaf}, LocalActor: "member", ResetEpoch: 1}
	require.NoError(t, c.Validate(leaf), "eight hubs plus a leaf remain supported")
	c.ServeDownstream = true
	require.Error(t, c.Validate(leaf), "a ninth serving hub is not supported")
}
