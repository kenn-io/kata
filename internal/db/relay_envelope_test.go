package db_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R4 authenticated hop identity is supplied by the enrolled transport, while
// source UID/hash stay separate and immutable. A digest alone grants no access.
func TestRelayHopEnvelope(t *testing.T) {
	grant := db.RelayHopAuthority{BindingUID: "00000000000000000000000001", ProjectUID: "00000000000000000000000002", AuthorityUID: "00000000000000000000000003", SenderInstanceUID: "00000000000000000000000004", ReceiverInstanceUID: "00000000000000000000000005", Epoch: 2}
	source := []byte(`{ "actor":"assistant", "payload": {"title":"Task"} }`)
	input := db.RelayEnvelope{Version: 1, BindingUID: grant.BindingUID, ProjectUID: grant.ProjectUID, AuthorityUID: grant.AuthorityUID, SenderInstanceUID: grant.SenderInstanceUID, ReceiverInstanceUID: grant.ReceiverInstanceUID, Epoch: grant.Epoch, Sequence: 7, Stream: db.RelayStreamEvent, Path: []string{"00000000000000000000000006", grant.SenderInstanceUID}, SourceUID: "00000000000000000000000007", SourceHash: strings.Repeat("a", 64), Body: source}
	envelope, err := db.SealRelayEnvelope(input)
	require.NoError(t, err)
	require.NoError(t, db.ValidateRelayEnvelope(grant, envelope))
	require.Equal(t, source, envelope.Body)
	require.Equal(t, input.SourceUID, envelope.SourceUID)
	require.Equal(t, input.SourceHash, envelope.SourceHash)
	require.Empty(t, input.Digest, "sealing does not mutate caller state")
	for _, change := range []func(*db.RelayEnvelope){
		func(e *db.RelayEnvelope) { e.BindingUID = "00000000000000000000000009" },
		func(e *db.RelayEnvelope) { e.ProjectUID = "00000000000000000000000009" },
		func(e *db.RelayEnvelope) { e.AuthorityUID = "00000000000000000000000009" },
		func(e *db.RelayEnvelope) { e.SenderInstanceUID = "00000000000000000000000009" },
		func(e *db.RelayEnvelope) { e.ReceiverInstanceUID = "00000000000000000000000009" },
		func(e *db.RelayEnvelope) { e.Epoch++ },
	} {
		wrong := envelope
		change(&wrong)
		wrong, err = db.SealRelayEnvelope(wrong)
		if err == nil {
			require.Error(t, db.ValidateRelayEnvelope(grant, wrong), "a caller can recompute digests but cannot change the admitted hop")
		}
	}
	changed := envelope
	changed.Body = slices.Clone(envelope.Body)
	changed.Body[1] ^= 1
	require.Error(t, db.ValidateRelayEnvelope(grant, changed))
	changed = envelope
	changed.Sequence++
	require.Error(t, db.ValidateRelayEnvelope(grant, changed))
	for _, path := range [][]string{{grant.SenderInstanceUID, grant.SenderInstanceUID}, {grant.ReceiverInstanceUID, grant.SenderInstanceUID}, {"00000000000000000000000006"}} {
		changed = envelope
		changed.Path = path
		_, err = db.SealRelayEnvelope(changed)
		require.Error(t, err)
	}
	changed = envelope
	changed.Stream = "unknown"
	_, err = db.SealRelayEnvelope(changed)
	require.Error(t, err)
	changed = envelope
	changed.Sequence = 0
	_, err = db.SealRelayEnvelope(changed)
	require.Error(t, err)
	changed = envelope
	changed.Epoch = 0
	_, err = db.SealRelayEnvelope(changed)
	require.Error(t, err)
	changed = envelope
	changed.Path = make([]string, 9)
	_, err = db.SealRelayEnvelope(changed)
	require.Error(t, err)
	again, err := db.SealRelayEnvelope(input)
	require.NoError(t, err)
	require.Equal(t, envelope.Digest, again.Digest)
}

func FuzzRelayEnvelopeExactBytes(f *testing.F) {
	f.Add([]byte("opaque source bytes"), uint16(7))
	f.Add([]byte{0xff, 0, 0xc0, 0x80}, uint16(65535))
	f.Add([]byte{}, uint16(0))
	f.Fuzz(func(t *testing.T, body []byte, sequence uint16) {
		// Exact-byte preservation is independent of UTF-8/JSON validity. The content
		// codec validates that separately; this layer never silently reencodes it.
		grant := db.RelayHopAuthority{BindingUID: "00000000000000000000000001", ProjectUID: "00000000000000000000000002", AuthorityUID: "00000000000000000000000003", SenderInstanceUID: "00000000000000000000000004", ReceiverInstanceUID: "00000000000000000000000005", Epoch: 2}
		input := db.RelayEnvelope{Version: 1, BindingUID: grant.BindingUID, ProjectUID: grant.ProjectUID, AuthorityUID: grant.AuthorityUID, SenderInstanceUID: grant.SenderInstanceUID, ReceiverInstanceUID: grant.ReceiverInstanceUID, Epoch: grant.Epoch, Sequence: int64(sequence) + 1, Stream: db.RelayStreamEvent, Path: []string{grant.SenderInstanceUID}, SourceUID: "00000000000000000000000007", SourceHash: strings.Repeat("a", 64), Body: body}
		envelope, err := db.SealRelayEnvelope(input)
		if len(body) > db.MaxRelayEnvelopeBytes {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		require.NoError(t, db.ValidateRelayEnvelope(grant, envelope))
		require.Equal(t, body, envelope.Body)
		envelope.Body = append(slices.Clone(envelope.Body), 0)
		require.Error(t, db.ValidateRelayEnvelope(grant, envelope), "the same identity cannot acknowledge changed bytes")
	})
}

func TestRelayEnvelopeBoundsAndCopy(t *testing.T) {
	grant := db.RelayHopAuthority{BindingUID: "00000000000000000000000001", ProjectUID: "00000000000000000000000002", AuthorityUID: "00000000000000000000000003", SenderInstanceUID: "00000000000000000000000004", ReceiverInstanceUID: "00000000000000000000000005", Epoch: 1}
	input := db.RelayEnvelope{Version: 1, BindingUID: grant.BindingUID, ProjectUID: grant.ProjectUID, AuthorityUID: grant.AuthorityUID, SenderInstanceUID: grant.SenderInstanceUID, ReceiverInstanceUID: grant.ReceiverInstanceUID, Epoch: 1, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{grant.SenderInstanceUID}, SourceUID: "00000000000000000000000007", SourceHash: strings.Repeat("a", 64), Body: []byte{1, 2}}
	envelope, err := db.SealRelayEnvelope(input)
	require.NoError(t, err)
	input.Body[0] = 9
	input.Path[0] = grant.ReceiverInstanceUID
	require.Equal(t, []byte{1, 2}, envelope.Body)
	require.NoError(t, db.ValidateRelayEnvelope(grant, envelope))
	input = envelope
	input.Body = make([]byte, db.MaxRelayEnvelopeBytes+1)
	_, err = db.SealRelayEnvelope(input)
	require.Error(t, err)
	input.Body = input.Body[:db.MaxRelayEnvelopeBytes]
	bounded, err := db.SealRelayEnvelope(input)
	require.NoError(t, err)
	require.NoError(t, db.ValidateRelayEnvelope(grant, bounded))
	input.Body = nil
	input.Path = nil
	for i := range db.MaxRelayPathNodes - 1 {
		input.Path = append(input.Path, fmt.Sprintf("%026d", 100+i))
	}
	input.Path = append(input.Path, grant.SenderInstanceUID)
	_, err = db.SealRelayEnvelope(input)
	require.NoError(t, err)
	input.Path = append([]string{"00000000000000000000000200"}, input.Path...)
	_, err = db.SealRelayEnvelope(input)
	require.Error(t, err)
	input = envelope
	input.Stream = db.RelayStreamReceipt
	receipt, err := db.SealRelayEnvelope(input)
	require.NoError(t, err)
	require.NoError(t, db.ValidateRelayEnvelope(grant, receipt))
	input.Stream = db.RelayStreamArtifact
	input.SourceUID = input.SourceHash
	artifact, err := db.SealRelayEnvelope(input)
	require.NoError(t, err)
	require.NoError(t, db.ValidateRelayEnvelope(grant, artifact))
	input.SourceUID = strings.Repeat("b", 64)
	_, err = db.SealRelayEnvelope(input)
	require.Error(t, err)
}
