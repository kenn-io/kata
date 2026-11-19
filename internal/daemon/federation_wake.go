package daemon

import (
	"context"
)

// ForwardFederationWakes wakes the federation runner for every broadcast event
// this daemon wrote itself, until ctx ends. Each wake names the event's
// project, so the runner syncs only that project's spoke binding; an event
// without a project wakes every spoke. Events the runner pulled from a hub are
// broadcast too, for clients and hooks, but they are the runner's own work and
// must not schedule another pass; pushes accepted from downstream relays wake
// the runner from their handler.
//
// When the subscription overflows, the broadcaster drops it and the events it
// missed are unknown, so the forwarder wakes every spoke and subscribes again.
func ForwardFederationWakes(
	ctx context.Context,
	broadcaster *EventBroadcaster,
	localInstanceUID string,
	wakeProject func(projectID int64),
	wakeAll func(),
) {
	for ctx.Err() == nil {
		sub := broadcaster.Subscribe(SubFilter{})
		forwardFederationWakes(ctx, sub, localInstanceUID, wakeProject, wakeAll)
		sub.Unsub()
		if ctx.Err() == nil {
			wakeAll()
		}
	}
}

// forwardFederationWakes returns when ctx ends or the subscription closes.
func forwardFederationWakes(ctx context.Context, sub Subscription, localInstanceUID string, wakeProject func(int64), wakeAll func()) {
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
			wakeProject(msg.Event.ProjectID)
		}
	}
}
