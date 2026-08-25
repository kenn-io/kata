package daemon

import (
	"sync"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
)

// channelBuffer is the per-subscriber send buffer. Full channels trigger
// overflow disconnect (Broadcast closes the channel and removes the
// subscriber). Plan 7 may expose this as a `kata config` knob; for now it's
// a const matching spec §11.
const channelBuffer = 256

// StreamKind discriminates the two envelope shapes a subscriber can receive.
// Producers should use the named constants below so they share one vocabulary
// instead of scattering string literals that runLivePhase can silently drop.
type StreamKind string

const (
	// StreamKindEvent carries a durable event wakeup; Event is non-nil.
	StreamKindEvent StreamKind = "event"
	// StreamKindReset carries a purge reset signal; ResetID is non-zero and
	// Event is nil.
	StreamKindReset StreamKind = "reset"
)

// StreamMsg is the envelope on each subscriber's channel. Build one with
// NewEventMsg or NewResetMsg: the constructors are what keep Kind and the
// payload fields consistent.
type StreamMsg struct {
	Kind      StreamKind
	Event     *db.Event // non-nil iff Kind == StreamKindEvent
	ResetID   int64     // non-zero iff Kind == StreamKindReset
	ProjectID int64     // 0 = cross-project; used for filter matching
}

// NewEventMsg builds an event envelope. ev is taken by value and the envelope
// points at that copy, so callers can pass a loop variable or a slice element
// without the next iteration mutating an already-broadcast frame.
func NewEventMsg(projectID int64, ev db.Event) StreamMsg {
	return StreamMsg{Kind: StreamKindEvent, Event: &ev, ProjectID: projectID}
}

// NewResetMsg builds a purge-reset envelope. Resets are terminal for an SSE
// subscriber: the handler writes the frame and returns.
func NewResetMsg(projectID, resetAfterID int64) StreamMsg {
	return StreamMsg{Kind: StreamKindReset, ResetID: resetAfterID, ProjectID: projectID}
}

// SubFilter restricts which broadcasts a subscriber receives. ProjectID 0
// (zero value) means cross-project — every event flows through.
type SubFilter struct {
	ProjectID int64
}

func (f SubFilter) matches(msg StreamMsg) bool {
	if f.ProjectID == 0 {
		return true
	}
	return msg.ProjectID == f.ProjectID
}

// Subscription is the handle returned by Subscribe. Caller must call Unsub()
// when done. Ch is closed by the broadcaster on overflow disconnect or by
// Unsub on caller exit. Unsub is safe to call multiple times.
type Subscription struct {
	Ch    <-chan StreamMsg
	Unsub func()
}

// EventBroadcaster fans out wakeups and reset signals to subscribers. It
// holds no DB reference; the SSE handler captures its own high-water mark
// (via db.MaxEventID) after Subscribe.
type EventBroadcaster struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]*subscriber
}

type subscriber struct {
	ch     chan StreamMsg
	filter SubFilter
}

// NewEventBroadcaster constructs an empty broadcaster. The daemon owns one
// instance; its lifetime matches the server process.
func NewEventBroadcaster() *EventBroadcaster {
	return &EventBroadcaster{subs: map[int]*subscriber{}}
}

// Subscribe registers a new subscriber with the given filter. Returned
// Subscription holds a read-only Ch and an Unsub closure that's safe to call
// repeatedly.
//
// Callers must invoke Unsub (typically via defer) to release resources; the
// only automatic cleanup is overflow eviction when the channel fills.
func (b *EventBroadcaster) Subscribe(filter SubFilter) Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan StreamMsg, channelBuffer)
	b.subs[id] = &subscriber{ch: ch, filter: filter}
	return Subscription{
		Ch: ch,
		Unsub: func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if sub, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(sub.ch)
			}
		},
	}
}

// Broadcast fans msg out to every matching subscriber. Sends are non-blocking;
// when a subscriber's buffer is full the broadcaster closes its channel and
// removes it (overflow disconnect). The SSE handler reading on the closed
// channel returns; the client reconnects with Last-Event-ID and resumes via
// the durable replay path.
//
// Single full Lock keeps the implementation small; single-user daemon
// throughput doesn't justify an RLock+Lock dance.
func (b *EventBroadcaster) Broadcast(msg StreamMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, sub := range b.subs {
		if !sub.filter.matches(msg) {
			continue
		}
		select {
		case sub.ch <- msg:
		default:
			close(sub.ch)
			delete(b.subs, id)
		}
	}
}

// EventPublisher owns the rule that a durable event reaches both the SSE
// broadcaster and the hook sink. Before it existed the pairing was two
// statements repeated at ~30 call sites, and omitting the second one compiled,
// passed every handler test, and showed up only as webhooks that never fired.
//
// The zero value is not usable; build one with NewEventPublisher.
type EventPublisher struct {
	Broadcaster *EventBroadcaster
	Hooks       hooks.Sink
}

// NewEventPublisher fills absent sinks with the same defaults the daemon
// applies elsewhere: a private broadcaster nobody subscribes to, and the noop
// hook sink.
func NewEventPublisher(broadcaster *EventBroadcaster, sink hooks.Sink) EventPublisher {
	if broadcaster == nil {
		broadcaster = NewEventBroadcaster()
	}
	if sink == nil {
		sink = hooks.NewNoop()
	}
	return EventPublisher{Broadcaster: broadcaster, Hooks: sink}
}

// Event fans one durable event out to both sinks. Broadcast happens first,
// matching every call site this replaced.
//
// Callers keep their own state-transition guards (`changed && evt != nil`):
// "was there a transition" is a different question from "is the event
// non-nil", and folding it in here would change when hooks fire.
func (p EventPublisher) Event(projectID int64, ev db.Event) {
	p.EventFrom(projectID, ev, nil)
}

// EventFrom fans one durable event out to both sinks, using acquire for hook
// work caused by an already-admitted parent operation. A nil acquire preserves
// the ordinary sink behavior used by request handlers.
func (p EventPublisher) EventFrom(projectID int64, ev db.Event, acquire activity.Admission) {
	p.Broadcaster.Broadcast(NewEventMsg(projectID, ev))
	hooks.EnqueueFrom(p.Hooks, ev, acquire)
}

// Events publishes evs in slice order. Order is contractual: EditIssueAtomic
// emits issue.updated -> priority -> links_changed and clients rely on it.
func (p EventPublisher) Events(projectID int64, evs []db.Event) {
	for i := range evs {
		p.Event(projectID, evs[i])
	}
}

// EventsFrom publishes evs in slice order, preserving the parent admission
// source for each caused hook job.
func (p EventPublisher) EventsFrom(projectID int64, evs []db.Event, acquire activity.Admission) {
	for i := range evs {
		p.EventFrom(projectID, evs[i], acquire)
	}
}

// EventsByProject publishes evs in slice order using each durable event's own
// project. Claim operations use this because their opportunistic expiry pass
// can emit events for projects other than the claim currently being handled.
func (p EventPublisher) EventsByProject(evs []db.Event) {
	for i := range evs {
		p.Event(evs[i].ProjectID, evs[i])
	}
}

// Reset broadcasts a purge reset. It deliberately does not enqueue: a reset is
// a stream-control frame, not a durable event, and hooks have never received
// one.
func (p EventPublisher) Reset(projectID, resetAfterID int64) {
	p.Broadcaster.Broadcast(NewResetMsg(projectID, resetAfterID))
}
