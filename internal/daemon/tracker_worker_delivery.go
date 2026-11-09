package daemon

import (
	"context"
	"sync"
	"time"
)

// Reply delivery is independent of polling. Six ordinary uploads cannot consume
// the two reserved metadata reply slots. Every child is bounded and joined;
// failure retains bytes for the next tick instead of spinning or losing proof.
func (r *TrackerRuntime) runTrackerReplyDelivery(ctx context.Context, cfg ServerConfig, w *trackerOwnerWorker) {
	runTrackerReplyDelivery(ctx, w, func(ctx context.Context, call *trackerOwnerCall) {
		project, err := cfg.DB.ProjectByUID(ctx, call.command.Ticket.Ticket.ProjectUID)
		if err != nil {
			return
		}
		binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
		if err != nil || binding.RelayConfig == nil {
			return
		}
		presence, _ := r.workerPresence(ctx, project, binding, w)
		flushTrackerWorkerReply(ctx, presence, w, call, func(ctx context.Context, body trackerWorkerBody, action string) error {
			_, err := forwardTrackerWorker(ctx, cfg, project, body, action)
			return err
		})
	})
}
func runTrackerReplyDelivery(ctx context.Context, w *trackerOwnerWorker, deliver func(context.Context, *trackerOwnerCall)) {
	ordinary := make(chan struct{}, 6)
	reserved := make(chan struct{}, 2)
	var pending sync.WaitGroup
	defer pending.Wait()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-w.replyReady:
		}
		jobs := []*trackerOwnerCall{}
		w.mu.Lock()
		for _, call := range w.calls {
			if !call.complete || call.sent || call.flushing {
				continue
			}
			slots := ordinary
			if workerMetadataMethod(call.command.Ticket.Ticket.Method) {
				slots = reserved
			}
			select {
			case slots <- struct{}{}:
				call.flushing = true
				jobs = append(jobs, call)
			default:
			}
		}
		w.mu.Unlock()
		for _, call := range jobs {
			pending.Add(1)
			go func() {
				defer pending.Done()
				slots := ordinary
				if workerMetadataMethod(call.command.Ticket.Ticket.Method) {
					slots = reserved
				}

				request, cancel := context.WithTimeout(ctx, 30*time.Second)
				deliver(request, call)
				cancel()
				w.mu.Lock()
				sent := call.sent
				if !sent {
					call.flushing = false
				}
				w.mu.Unlock()
				<-slots
				if sent {
					w.wakeReplies()
				}

			}()
		}
	}
}
