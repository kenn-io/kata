package daemon

import (
	"context"
	"go.kenn.io/kata/internal/tracker"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Contract: a stalled first project cannot block available network slots or
// impose a whole-pass barrier on later healthy enrolled projects.
func TestTrackerPollSchedulerProgressesWhileEarlierProjectIsStalled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan int64, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTrackerPollScheduler(ctx, time.Hour, 2,
			func(context.Context) ([]int64, error) { return []int64{1, 2, 3}, nil },
			func(ctx context.Context, id int64) {
				started <- id
				if id == 1 {
					<-ctx.Done()
				}
			})
	}()
	seen := map[int64]bool{}
	for len(seen) < 3 {
		select {
		case id := <-started:
			require.False(t, seen[id], "one queued/in-flight poll per project")
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("healthy project waited for stalled earlier poll")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not join canceled polls")
	}
}

// Contract: six stalled ordinary uploads leave completion
// scheduling and the two reserved metadata delivery slots available.
func TestTrackerReplyDeliveryProgressesIndependentlyAndReservesMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := newTrackerOwnerWorker(ctx)
	started := make(chan string, 8)
	ordinaryStarted := make(chan struct{}, 6)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTrackerReplyDelivery(ctx, w, func(ctx context.Context, call *trackerOwnerCall) {
			method := call.command.Ticket.Ticket.Method
			started <- method
			if method == "notion.pages" {
				ordinaryStarted <- struct{}{}
				<-ctx.Done()
			}
			w.mu.Lock()
			call.sent = true
			delete(w.calls, call.command.Ticket.Ticket.CallUID)
			w.mu.Unlock()
		})
	}()
	w.mu.Lock()
	for i := 0; i < 6; i++ {
		ticket := trackerWorkerTicketFixtureForResult()
		ticket.CallUID = workerSyntheticUID(500 + i)
		w.calls[ticket.CallUID] = &trackerOwnerCall{command: trackerWorkerCommand{Ticket: tracker.SignedWorkerTicket{Ticket: ticket}}, complete: true}
	}
	w.mu.Unlock()
	w.wakeReplies()
	for i := 0; i < 6; i++ {
		select {
		case <-ordinaryStarted:
		case <-time.After(time.Second):
			t.Fatal("ordinary upload was not scheduled")
		}
	}
	w.mu.Lock()
	ticket := trackerWorkerTicketFixtureForResult()
	ticket.CallUID = workerSyntheticUID(600)
	ticket.Method = "notion.receipt"
	w.calls[ticket.CallUID] = &trackerOwnerCall{command: trackerWorkerCommand{Ticket: tracker.SignedWorkerTicket{Ticket: ticket}}, complete: true}
	w.mu.Unlock()
	w.wakeReplies()
	for {
		select {
		case method := <-started:
			if method == "notion.receipt" {
				cancel()
				select {
				case <-done:
					return
				case <-time.After(time.Second):
					t.Fatal("delivery children were not joined")
				}
			}
		case <-time.After(time.Second):
			t.Fatal("reserved receipt waited for ordinary uploads")
		}
	}
}
