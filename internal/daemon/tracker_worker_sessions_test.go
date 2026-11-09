package daemon

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"testing"
	"time"
)

func TestTrackerOwnerSessionsReuseRunAndBoundBusyLifetime(t *testing.T) {
	now := time.Now()
	manager := newTrackerOwnerWorker(t.Context())
	ticket := trackerWorkerTicketFixtureForResult()
	session, release, err := manager.acquire(ticket, now)
	require.NoError(t, err)
	_, _, err = manager.acquire(ticket, now)
	require.Error(t, err, "a second method cannot run concurrently on one credential session")
	manager.prune(now.Add(31 * time.Second))
	require.NoError(t, session.ctx.Err(), "idle expiry does not cancel an admitted busy method")
	release()
	same, release, err := manager.acquire(ticket, now.Add(20*time.Second))
	require.NoError(t, err)
	require.Same(t, session, same)
	release()
	other := ticket
	other.RunUID = other.CallUID
	_, _, err = manager.acquire(other, now.Add(21*time.Second))
	require.Error(t, err, "another run cannot borrow a retained provider session")
	manager.prune(now.Add(26 * time.Minute))
	require.Error(t, session.ctx.Err(), "hard lifetime does not extend through activity")
	fresh, release, err := manager.acquire(other, now.Add(26*time.Minute))
	require.NoError(t, err)
	require.NotSame(t, session, fresh)
	release()
	manager.closeAll()
	require.Error(t, fresh.ctx.Err())
}

// Contract: the owned session supplies lifetime, while the mapped invocation
// supplies all native authorization values/fences. Revocation between the
// visibility read and final transaction must still reject the write.
func TestTrackerWorkerMethodContextRetainsLiveTransactionAuthority(t *testing.T) {
	allowed := true
	checks := 0
	base := db.WithAuthorizedProjects(t.Context(), []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	base = db.WithAdditionalTransactionFence(base, func(context.Context, db.Transaction) error {
		checks++
		if !allowed {
			return tracker.ErrDenied
		}
		return nil
	})
	sessionCtx, sessionCancel := context.WithCancel(t.Context())
	defer sessionCancel()
	session := &trackerOwnerSession{ctx: sessionCtx, hardDeadline: time.Now().Add(time.Minute)}
	method, cleanup := trackerWorkerMethodContext(base, session, trackerWorkerTicketFixtureForResult())
	defer cleanup()
	projects, restricted := db.AuthorizedProjects(method)
	require.True(t, restricted)
	require.Equal(t, []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"}, projects)
	require.NoError(t, db.ApplyTransactionFence(method, nil))
	allowed = false
	require.ErrorIs(t, db.ApplyTransactionFence(method, nil), tracker.ErrDenied)
	require.Equal(t, 2, checks)
	sessionCancel()
	select {
	case <-method.Done():
	case <-time.After(time.Second):
		t.Fatal("session close did not cancel its method")
	}
}

func TestTrackerWorkerReplyRetryRetainsBytesUntilAcknowledged(t *testing.T) {
	manager := newTrackerOwnerWorker(t.Context())
	ticket := trackerWorkerTicketFixtureForResult()
	data := bytes.Repeat([]byte("x"), tracker.WorkerChunkBytes+1)
	manifest, chunks, err := tracker.SplitWorkerResult(ticket, data)
	require.NoError(t, err)
	call := &trackerOwnerCall{command: trackerWorkerCommand{Ticket: tracker.SignedWorkerTicket{Ticket: ticket}}, complete: true, flushing: true, held: len(data), reply: &trackerWorkerReply{CallUID: ticket.CallUID, SessionUID: ticket.SessionUID, Code: "ok", Manifest: &manifest}, chunks: chunks}
	manager.calls[ticket.CallUID] = call
	manager.retained = len(data)
	presence := tracker.WorkerPresence{ProjectUID: ticket.ProjectUID, OwnerInstanceUID: ticket.Selection.OwnerInstanceUID}
	attempts := 0
	flushTrackerWorkerReply(t.Context(), presence, manager, call, func(context.Context, trackerWorkerBody, string) error { attempts++; return tracker.ErrUnavailable })
	require.Equal(t, 1, attempts)
	require.True(t, call.flushing, "failed inner send does not release its owner's claim")
	require.Equal(t, len(data), manager.retained)
	require.Contains(t, manager.calls, ticket.CallUID)
	require.Equal(t, chunks, call.chunks)
	delivered := []string{}
	flushTrackerWorkerReply(t.Context(), presence, manager, call, func(_ context.Context, body trackerWorkerBody, action string) error {
		delivered = append(delivered, action)
		require.Equal(t, presence, body.Presence)
		if action == "reply" {
			require.Equal(t, &manifest, body.Reply.Manifest)
		}
		return nil
	})
	require.Equal(t, []string{"reply", "chunk", "chunk"}, delivered)
	require.Zero(t, manager.retained)
	require.NotContains(t, manager.calls, ticket.CallUID)
}

// Contract: the delivery dispatcher owns its claim through the whole job. An
// inner failed send must not publish a free claim before its caller finishes.
func TestTrackerWorkerReplyFailureRetainsDispatcherClaim(t *testing.T) {
	w := newTrackerOwnerWorker(t.Context())
	ticket := trackerWorkerTicketFixtureForResult()
	call := &trackerOwnerCall{command: trackerWorkerCommand{Ticket: tracker.SignedWorkerTicket{Ticket: ticket}}, complete: true, flushing: true, held: 3, reply: &trackerWorkerReply{CallUID: ticket.CallUID, SessionUID: ticket.SessionUID, Code: "ok", Data: []byte(`123`)}}
	w.calls[ticket.CallUID] = call
	w.retained = 3
	flushTrackerWorkerReply(t.Context(), tracker.WorkerPresence{}, w, call, func(context.Context, trackerWorkerBody, string) error { return tracker.ErrUnavailable })
	require.True(t, call.flushing, "only the dispatcher may release this in-flight claim")
	require.False(t, call.sent)
	require.NotNil(t, call.reply)
	require.Equal(t, 3, w.retained)
}

func TestTrackerWorkerMethodBudgetCannotBorrowLongerSession(t *testing.T) {
	session := &trackerOwnerSession{ctx: t.Context(), hardDeadline: time.Now().Add(25 * time.Minute)}
	ticket := trackerWorkerTicketFixtureForResult()
	ticket.ExpiresAt = time.Now().Add(-trackerWorkerReplyGrace - time.Second).Unix()
	method, cleanup := trackerWorkerMethodContext(t.Context(), session, ticket)
	defer cleanup()
	require.ErrorIs(t, method.Err(), context.DeadlineExceeded)
	require.NoError(t, session.ctx.Err(), "one expired call does not authorize closing unrelated session work")
}
