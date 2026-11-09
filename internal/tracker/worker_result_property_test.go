package tracker

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Contract: a complete manifest returns the original bytes, never a partial
// cursor result. The full 64 MiB domain is materialized, one case at a time;
// the case budget bounds CI allocation churn rather than narrowing sizes.
func TestWorkerResultExactRoundTripProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		size := hegel.Draw(ht, hegel.Integers(1, WorkerResultBytes))
		seed := hegel.Draw(ht, hegel.Integers(0, 255))
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i*31 + seed) //nolint:gosec // Repeating byte pattern is intentional.
		}
		ticket := workerTicketFixture(time.Unix(1900000000, 0))
		manifest, chunks, err := SplitWorkerResult(ticket, payload)
		require.NoError(ht, err)
		assembler, err := NewWorkerResultAssembler(manifest, ticket)
		require.NoError(ht, err)
		defer assembler.Close()
		for _, chunk := range chunks {
			require.NoError(ht, assembler.Add(chunk))
			// An identical retransmission is delivery, not a second chunk.
			require.NoError(ht, assembler.Add(chunk))
		}
		actual, err := assembler.Complete()
		require.NoError(ht, err)
		require.True(ht, bytes.Equal(payload, actual), "assembly must preserve every byte")
	}, hegel.WithTestCases(24))
}

// Contract: neither split input nor completed output aliases retained state.
func TestWorkerResultBufferOwnershipProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		size := hegel.Draw(ht, hegel.Integers(1, WorkerChunkBytes+1))
		value := byte(hegel.Draw(ht, hegel.Integers(0, 255))) //nolint:gosec // Generator bounds the value to one byte.
		input := bytes.Repeat([]byte{value}, size)
		manifest, chunks, err := SplitWorkerResult(workerTicketFixture(time.Unix(1900000000, 0)), input)
		require.NoError(ht, err)
		clear(input)
		a, err := NewWorkerResultAssembler(manifest, workerTicketFixture(time.Unix(1900000000, 0)))
		require.NoError(ht, err)
		defer a.Close()
		for i := range chunks {
			require.NoError(ht, a.Add(chunks[i]))
			clear(chunks[i].Data)
		}
		first, err := a.Complete()
		require.NoError(ht, err)
		for i := range first {
			first[i] = value ^ 255
		}
		second, err := a.Complete()
		require.NoError(ht, err)
		require.Equal(ht, bytes.Repeat([]byte{value}, size), second)
	}, hegel.WithTestCases(100))
}

// Contract: malformed delivery cannot release even a correctly hashed prefix.
func TestWorkerResultRejectsCorruptDeliveryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ticket := workerTicketFixture(time.Unix(1900000000, 0))
		size := hegel.Draw(ht, hegel.Integers(1, WorkerChunkBytes*2))
		m, chunks, err := SplitWorkerResult(ticket, bytes.Repeat([]byte{7}, size))
		require.NoError(ht, err)
		a, err := NewWorkerResultAssembler(m, ticket)
		require.NoError(ht, err)
		defer a.Close()
		bad := chunks[0]
		switch hegel.Draw(ht, hegel.Integers(0, 6)) {
		case 0:
			bad.RootInstanceUID = ticket.Selection.OwnerInstanceUID
		case 1:
			bad.ProjectUID = ticket.RunUID
		case 2:
			bad.OwnerInstanceUID = ticket.RootInstanceUID
		case 3:
			bad.CallUID = ticket.RunUID
		case 4:
			bad.SessionUID = ticket.RunUID
		case 5:
			bad.Ordinal = len(chunks)
		case 6:
			bad.Data = bytes.Clone(bad.Data)
			bad.Data[0] ^= 255
		}
		require.Error(ht, a.Add(bad))
		_, err = a.Complete()
		require.Error(ht, err)
	}, hegel.WithTestCases(100))
}
