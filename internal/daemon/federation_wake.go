package daemon

import (
	"context"
)

// ForwardFederationWakes wakes the federation runner for every broadcast event
// this daemon wrote itself, until ctx ends. Events the runner pulled from a hub
// are broadcast too, for clients and hooks, but they are the runner's own work
// and must not restart a pass over every spoke while a project catches up;
// pushes accepted from downstream relays wake the runner from their handler.
func ForwardFederationWakes(ctx context.Context, broadcaster *EventBroadcaster, localInstanceUID string, wake func()) {
	sub := broadcaster.Subscribe(SubFilter{})
	defer sub.Unsub()
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
			wake()
		}
	}
}
