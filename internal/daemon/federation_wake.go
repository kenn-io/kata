package daemon

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// SubscribeFederationWakes subscribes to broadcaster now and returns the loop
// that wakes the federation runner for every broadcast event store wrote
// itself, until ctx ends. Subscribing before the runner starts means no local
// write after this call goes unnoticed.
//
// Each wake names the event's project, so the runner syncs only that
// project's spoke binding. A move also wakes its source project, whose
// binding may still owe the hub the moved issue's earlier events. An event
// without a project wakes every spoke. Events the runner pulled from a hub
// are broadcast too, for clients and hooks, but they are the runner's own
// work and must not schedule another pass; pushes accepted from downstream
// relays wake the runner from their handler.
//
// When the subscription overflows, the broadcaster drops it and the events it
// missed are unknown. The loop subscribes again and then wakes every spoke:
// that pass covers every write the old subscription missed, and the new
// subscription sees every write after it.
func SubscribeFederationWakes(
	broadcaster *EventBroadcaster,
	store db.Storage,
	wakeProject func(projectID int64),
	wakeAll func(),
) func(ctx context.Context) {
	sub := broadcaster.Subscribe(SubFilter{})
	return func(ctx context.Context) {
		for {
			forwardFederationWakes(ctx, sub, store, wakeProject, wakeAll)
			sub.Unsub()
			if ctx.Err() != nil {
				return
			}
			sub = broadcaster.Subscribe(SubFilter{})
			wakeAll()
		}
	}
}

// forwardFederationWakes returns when ctx ends or the subscription closes.
func forwardFederationWakes(ctx context.Context, sub Subscription, store db.Storage, wakeProject func(int64), wakeAll func()) {
	localInstanceUID := store.InstanceUID()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-sub.Ch:
			if !ok {
				return
			}
			if msg.Kind != StreamKindEvent || msg.Event == nil || msg.Event.OriginInstanceUID != localInstanceUID {
				continue
			}
			if msg.Event.ProjectID == 0 {
				wakeAll()
				continue
			}
			if msg.movedFromProjectUID != "" {
				source, err := store.ProjectByUID(ctx, msg.movedFromProjectUID)
				if err != nil {
					// Without the source project, wake every spoke so the
					// source binding still runs.
					wakeAll()
					continue
				}
				wakeProject(source.ID)
			}
			wakeProject(msg.Event.ProjectID)
		}
	}
}
