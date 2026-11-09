package tracker

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestWorkerResultOnlyReleasesCompleteBoundManifest(t *testing.T) {
	ticket := workerTicketFixture(time.Unix(1900000000, 0))
	payload := bytes.Repeat([]byte("x"), WorkerChunkBytes+9)
	manifest, chunks, err := SplitWorkerResult(ticket, payload)
	require.NoError(t, err)
	require.Len(t, chunks, 2)
	assembler, err := NewWorkerResultAssembler(manifest, ticket)
	require.NoError(t, err)
	require.Error(t, assembler.Add(chunks[1]), "out of order")
	require.NoError(t, assembler.Add(chunks[0]))
	_, err = assembler.Complete()
	require.Error(t, err, "incomplete output cannot advance a cursor")
	changed := chunks[1]
	changed.CallUID = ticket.RunUID
	require.Error(t, assembler.Add(changed), "call identity is not inherited from payload order")
	changed = chunks[1]
	changed.Data = []byte("corrupt")
	require.Error(t, assembler.Add(changed), "tampering")
	require.NoError(t, assembler.Add(chunks[1]))
	result, err := assembler.Complete()
	require.NoError(t, err)
	require.Equal(t, payload, result)
	result[0] = 'z'
	again, err := assembler.Complete()
	require.NoError(t, err)
	require.Equal(t, byte('x'), again[0], "caller cannot alter retained result")
	assembler.Close()
	_, err = assembler.Complete()
	require.Error(t, err)
	manifest.Size = WorkerResultBytes + 1
	_, err = NewWorkerResultAssembler(manifest, ticket)
	require.Error(t, err, "no allocation for oversized manifest")
}
