package daemon

import "go.kenn.io/kata/internal/mcpevents"

// mcpEventsSubscriber adapts the daemon's EventBroadcaster to the wake source
// the MCP Events delivery worker expects.
type mcpEventsSubscriber struct{ broadcaster *EventBroadcaster }

// NewMCPEventsSubscriber returns the single wake source for the MCP Events
// worker. Every broadcast, including a purge reset, becomes one coalesced
// wake: the worker re-reads the database, so no payload is carried. The wake
// channel closes when the broadcaster drops the subscriber for falling behind
// (overflow) and when cancel is called; the worker then resubscribes and
// drains from the database.
func NewMCPEventsSubscriber(broadcaster *EventBroadcaster) mcpevents.Subscriber {
	return mcpEventsSubscriber{broadcaster: broadcaster}
}

func (s mcpEventsSubscriber) Subscribe() (<-chan struct{}, func()) {
	sub := s.broadcaster.Subscribe(SubFilter{})
	wake := make(chan struct{}, 1)
	go func() {
		defer close(wake)
		for range sub.Ch {
			select {
			case wake <- struct{}{}:
			default: // a wake is already pending
			}
		}
	}()
	return wake, sub.Unsub
}
