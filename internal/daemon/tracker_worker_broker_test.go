package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/tracker"
	"strings"
	"testing"
	"time"
)

// Contract: delivery is only to the selected owner/project, and a response
// cannot escape the exact signed call/session or wake another caller.
func TestTrackerWorkerBrokerExactDeliveryAndCancellation(t *testing.T) {
	b := newTrackerWorkerBroker()
	command := trackerWorkerCommandFixture()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pending, err := b.enqueue(ctx, command, false)
	require.NoError(t, err)
	require.Empty(t, b.poll(command.Ticket.Ticket.ProjectUID, "01ARZ3NDEKTSV4RRFFQ69G5FB9", time.Now()))
	require.Len(t, b.poll(command.Ticket.Ticket.ProjectUID, command.Ticket.Ticket.Selection.OwnerInstanceUID, time.Now()), 1)
	wrong := trackerWorkerReply{CallUID: command.Ticket.Ticket.CallUID, SessionUID: command.Ticket.Ticket.RunUID, Code: "ok", Data: jsontext.Value(`{"allowed":true}`)}
	require.Error(t, b.reply(command.Ticket.Ticket.ProjectUID, command.Ticket.Ticket.Selection.OwnerInstanceUID, wrong))
	wrong.SessionUID = command.Ticket.Ticket.SessionUID
	require.NoError(t, b.reply(command.Ticket.Ticket.ProjectUID, command.Ticket.Ticket.Selection.OwnerInstanceUID, wrong))
	select {
	case result := <-pending.result:
		require.Equal(t, "ok", result.Code)
	default:
		t.Fatal("response not delivered")
	}
	require.Empty(t, b.poll(command.Ticket.Ticket.ProjectUID, command.Ticket.Ticket.Selection.OwnerInstanceUID, time.Now()))
	b.release(command.Ticket.Ticket.CallUID)
	// A stopped caller releases only volatile data, never native write evidence.
	pending, err = b.enqueue(ctx, command, false)
	require.NoError(t, err)
	require.NotNil(t, pending)
	cancel()
	require.Empty(t, b.poll(command.Ticket.Ticket.ProjectUID, command.Ticket.Ticket.Selection.OwnerInstanceUID, time.Now()))
}
func trackerWorkerCommandFixture() trackerWorkerCommand {
	return trackerWorkerCommand{Ticket: tracker.SignedWorkerTicket{Ticket: tracker.WorkerTicket{ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Selection: tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", Generation: "first"}, CallUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", SessionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", RunUID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", ExpiresAt: time.Now().Add(30 * time.Second).Unix()}}, Arguments: []byte(`{}`)}
}
func TestTrackerWorkerBrokerReservesCompletionCapacity(t *testing.T) {
	b := newTrackerWorkerBroker()
	command := trackerWorkerCommandFixture()
	for i := 0; i < 64; i++ {
		c := command
		c.Ticket.Ticket.CallUID = workerSyntheticUID(i + 100)
		c.Ticket.Ticket.Selection.ConnectionUID = workerSyntheticUID(i + 1000)
		_, err := b.enqueue(t.Context(), c, false)
		require.NoError(t, err)
	}
	command.Ticket.Ticket.CallUID = workerSyntheticUID(300)
	_, err := b.enqueue(t.Context(), command, false)
	require.ErrorIs(t, err, tracker.ErrControlCapacity)
	_, err = b.enqueue(t.Context(), command, true)
	require.NoError(t, err, "metadata recovery has reserved slots")
}

func workerSyntheticUID(n int) string { return fmt.Sprintf("%026d", n) }

func TestTrackerWorkerBrokerWaitsForAllChunksAndChargesBudget(t *testing.T) {
	b := newTrackerWorkerBroker()
	command := trackerWorkerCommandFixture()
	// Manifest validity depends on a complete signed-call identity, independent
	// of signature verification performed by the owner transport boundary.
	command.Ticket.Ticket = trackerWorkerTicketFixtureForResult()
	pending, err := b.enqueue(t.Context(), command, false)
	require.NoError(t, err)
	data := bytes.Repeat([]byte("x"), tracker.WorkerChunkBytes+3)
	manifest, chunks, err := tracker.SplitWorkerResult(command.Ticket.Ticket, data)
	require.NoError(t, err)
	reply := trackerWorkerReply{CallUID: command.Ticket.Ticket.CallUID, SessionUID: command.Ticket.Ticket.SessionUID, Code: "ok", Manifest: &manifest}
	project, owner := manifest.ProjectUID, manifest.OwnerInstanceUID
	require.NoError(t, b.reply(project, owner, reply))
	require.NoError(t, b.reply(project, owner, reply), "manifest replay does not reset assembly or charge twice")
	select {
	case <-pending.result:
		t.Fatal("manifest alone completed a method")
	default:
	}
	require.NoError(t, b.chunk(project, owner, chunks[0]))
	require.NoError(t, b.chunk(project, owner, chunks[0]), "lost chunk acknowledgement is idempotent")
	require.NoError(t, b.chunk(project, owner, chunks[1]))
	select {
	case result := <-pending.result:
		require.Equal(t, data, []byte(result.Data))
	default:
		t.Fatal("complete result not delivered")
	}
	require.Greater(t, b.retained, 0)
	b.release(manifest.CallUID)
	require.Zero(t, b.retained)
}
func trackerWorkerTicketFixtureForResult() tracker.WorkerTicket {
	const root = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const project = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	return tracker.WorkerTicket{Version: 1, Purpose: tracker.WorkerExecute, KeyID: "root-key", RootInstanceUID: root, ProjectUID: project, Selection: tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Principal: tracker.Principal{AuthorityInstanceUID: root, Kind: tracker.SubjectService, Subject: "sync"}, RootActor: "service", Action: tracker.ActionRead, Resource: "notion:source", Provider: "notion", Method: "notion.pages", RunUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", CallUID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", SessionUID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", RootEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2", OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB3", ArgumentsDigest: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(30 * time.Second).Unix()}
}

// Contract: inline results share the same retained-byte budget as assembly.
// Reserving a maximum manifest needs no materialized64MiB fixture here.
func TestTrackerWorkerBrokerChargesInlineResultsAgainstAssemblyBudget(t *testing.T) {
	b := newTrackerWorkerBroker()
	command := trackerWorkerCommandFixture()
	command.Ticket.Ticket = trackerWorkerTicketFixtureForResult()
	_, err := b.enqueue(t.Context(), command, false)
	require.NoError(t, err)
	ticket := command.Ticket.Ticket
	digest := strings.Repeat("a", 64)
	manifest := tracker.WorkerResultManifest{Version: 1, RootInstanceUID: ticket.RootInstanceUID, ProjectUID: ticket.ProjectUID, OwnerInstanceUID: ticket.Selection.OwnerInstanceUID, RunUID: ticket.RunUID, CallUID: ticket.CallUID, SessionUID: ticket.SessionUID, Size: tracker.WorkerResultBytes, Digest: digest}
	for i := 0; i < tracker.WorkerResultBytes/tracker.WorkerChunkBytes; i++ {
		manifest.Chunks = append(manifest.Chunks, digest)
	}
	require.NoError(t, b.reply(ticket.ProjectUID, ticket.Selection.OwnerInstanceUID, trackerWorkerReply{CallUID: ticket.CallUID, SessionUID: ticket.SessionUID, Code: "ok", Manifest: &manifest}))
	inline := command
	inline.Ticket.Ticket.CallUID = workerSyntheticUID(800)
	_, err = b.enqueue(t.Context(), inline, false)
	require.NoError(t, err)
	reply := trackerWorkerReply{CallUID: inline.Ticket.Ticket.CallUID, SessionUID: ticket.SessionUID, Code: "ok", Data: jsontext.Value(`[1]`)}
	require.ErrorIs(t, b.reply(ticket.ProjectUID, ticket.Selection.OwnerInstanceUID, reply), tracker.ErrControlCapacity)
	require.Equal(t, tracker.WorkerProjectBytes, b.retained)
	b.release(ticket.CallUID)
	require.NoError(t, b.reply(ticket.ProjectUID, ticket.Selection.OwnerInstanceUID, reply))
	require.Equal(t, len(reply.Data), b.retained)
	b.release(reply.CallUID)
	b.release(reply.CallUID)
	require.Zero(t, b.retained, "release is exact and idempotent")
}

func TestTrackerWorkerBrokerConnectionLimitSpansProjectsAndGenerations(t *testing.T) {
	b := newTrackerWorkerBroker()
	command := trackerWorkerCommandFixture()
	for i := 0; i < 64; i++ {
		c := command
		c.Ticket.Ticket.CallUID = workerSyntheticUID(2000 + i)
		c.Ticket.Ticket.ProjectUID = workerSyntheticUID(3000 + i)
		c.Ticket.Ticket.Selection.Generation = fmt.Sprintf("generation-%d", i)
		_, err := b.enqueue(t.Context(), c, false)
		require.NoError(t, err)
	}
	command.Ticket.Ticket.CallUID = workerSyntheticUID(4000)
	command.Ticket.Ticket.ProjectUID = workerSyntheticUID(5000)
	command.Ticket.Ticket.Selection.Generation = "fresh"
	_, err := b.enqueue(t.Context(), command, false)
	require.ErrorIs(t, err, tracker.ErrControlCapacity, "generation/project changes cannot multiply one account's retained-call quota")
	_, err = b.enqueue(t.Context(), command, true)
	require.NoError(t, err, "recovery remains separately bounded")
}
