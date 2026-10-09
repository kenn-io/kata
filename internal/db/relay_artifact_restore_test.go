package db_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A restored issue needs a new offer while the old retired hop stays immutable.
func TestRelayArtifactRestorationIdentity(t *testing.T) {
	digest := strings.Repeat("a", 64)
	input := db.RelayEnvelope{Version: 1, BindingUID: "00000000000000000000000001", ProjectUID: "00000000000000000000000002", AuthorityUID: "00000000000000000000000003", SenderInstanceUID: "00000000000000000000000004", ReceiverInstanceUID: "00000000000000000000000005", Epoch: 1, Sequence: 7, Stream: db.RelayStreamArtifact, Path: []string{"00000000000000000000000004"}, SourceUID: digest + ":00000000000000000000000006", SourceHash: digest, Body: []byte(`{}`)}
	restored, err := db.SealRelayEnvelope(input)
	require.NoError(t, err)
	retry, err := db.SealRelayEnvelope(input)
	require.NoError(t, err)
	require.Equal(t, restored, retry)
	input.SourceUID = digest
	original, err := db.SealRelayEnvelope(input)
	require.NoError(t, err)
	require.NotEqual(t, original.Digest, restored.Digest)
	input.SourceUID = strings.Repeat("b", 64) + ":00000000000000000000000006"
	_, err = db.SealRelayEnvelope(input)
	require.Error(t, err)
	input.SourceUID = digest + ":invalid"
	_, err = db.SealRelayEnvelope(input)
	require.Error(t, err)
}
