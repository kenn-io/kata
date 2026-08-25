package daemon_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
)

func TestNewEventMsgCarriesACopyOfTheEvent(t *testing.T) {
	events := []db.Event{
		{ID: 1, ProjectID: 7, Type: "issue.created"},
		{ID: 2, ProjectID: 7, Type: "issue.updated"},
	}

	first := daemon.NewEventMsg(7, events[0])
	second := daemon.NewEventMsg(7, events[1])

	assert.Equal(t, daemon.StreamKindEvent, first.Kind)
	assert.Equal(t, int64(7), first.ProjectID)
	assert.Zero(t, first.ResetID)
	require.NotNil(t, first.Event)
	require.NotNil(t, second.Event)
	assert.Equal(t, int64(1), first.Event.ID)
	assert.Equal(t, int64(2), second.Event.ID)
	assert.NotSame(t, &events[0], first.Event,
		"the envelope must hold its own copy so a caller's slice element can be reused")
}

func TestNewResetMsgCarriesNoEvent(t *testing.T) {
	msg := daemon.NewResetMsg(7, 42)

	assert.Equal(t, daemon.StreamKindReset, msg.Kind)
	assert.Equal(t, int64(7), msg.ProjectID)
	assert.Equal(t, int64(42), msg.ResetID)
	assert.Nil(t, msg.Event, "a reset envelope must never carry an event")
}

type publisherSink struct {
	mu     sync.Mutex
	events []db.Event
}

func (s *publisherSink) Enqueue(evt db.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evt)
}

func (s *publisherSink) ids() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.events))
	for _, evt := range s.events {
		out = append(out, evt.ID)
	}
	return out
}

var _ hooks.Sink = (*publisherSink)(nil)

type publisherForkSink struct {
	mu      sync.Mutex
	regular []db.Event
	from    []db.Event
	onFrom  func(db.Event, activity.Admission)
}

func (s *publisherForkSink) Enqueue(evt db.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regular = append(s.regular, evt)
}

func (s *publisherForkSink) EnqueueFrom(evt db.Event, acquire activity.Admission) {
	s.mu.Lock()
	s.from = append(s.from, evt)
	onFrom := s.onFrom
	s.mu.Unlock()
	if onFrom != nil {
		onFrom(evt, acquire)
	}
}

func (s *publisherForkSink) regularIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.regular))
	for _, evt := range s.regular {
		ids = append(ids, evt.ID)
	}
	return ids
}

func (s *publisherForkSink) fromIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.from))
	for _, evt := range s.from {
		ids = append(ids, evt.ID)
	}
	return ids
}

func TestEventPublisherEventReachesBothSinks(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()
	sink := &publisherSink{}
	publisher := daemon.NewEventPublisher(broadcaster, sink)

	publisher.Event(7, db.Event{ID: 11, ProjectID: 7, Type: "issue.created"})

	msg := receiveMsg(t, sub.Ch, time.Second, "published event")
	require.NotNil(t, msg.Event)
	assert.Equal(t, daemon.StreamKindEvent, msg.Kind)
	assert.Equal(t, int64(11), msg.Event.ID)
	assert.Equal(t, int64(7), msg.ProjectID)
	assert.Equal(t, []int64{11}, sink.ids())
}

func TestEventPublisherEventsPreserveSliceOrderOnBothSinks(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()
	sink := &publisherSink{}
	publisher := daemon.NewEventPublisher(broadcaster, sink)

	publisher.Events(7, []db.Event{
		{ID: 31, ProjectID: 7, Type: "issue.updated"},
		{ID: 32, ProjectID: 7, Type: "issue.priority_changed"},
		{ID: 33, ProjectID: 7, Type: "issue.links_changed"},
	})

	var broadcastIDs []int64
	for range 3 {
		msg := receiveMsg(t, sub.Ch, time.Second, "published event")
		require.NotNil(t, msg.Event)
		broadcastIDs = append(broadcastIDs, msg.Event.ID)
	}
	assert.Equal(t, []int64{31, 32, 33}, broadcastIDs,
		"multi-event emission order is contractual (issue.updated -> priority -> links_changed)")
	assert.Equal(t, []int64{31, 32, 33}, sink.ids())
}

func TestEventPublisherEventsFromBroadcastsBeforeForkAwareHooksInOrder(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: 7})
	defer sub.Unsub()
	var broadcastIDs []int64
	var acquired, released int
	sink := &publisherForkSink{}
	sink.onFrom = func(_ db.Event, acquire activity.Admission) {
		select {
		case msg := <-sub.Ch:
			require.NotNil(t, msg.Event)
			broadcastIDs = append(broadcastIDs, msg.Event.ID)
		default:
			t.Fatal("hook admission ran before the event was broadcast")
		}
		lease, admitted := acquire()
		require.True(t, admitted)
		require.NotNil(t, lease)
		acquired++
		lease.Release()
	}
	publisher := daemon.NewEventPublisher(broadcaster, sink)

	publisher.EventsFrom(7, []db.Event{
		{ID: 51, ProjectID: 7, Type: "claim.expired"},
		{ID: 52, ProjectID: 7, Type: "claim.expired"},
	}, func() (*activity.Lease, bool) {
		return activity.NewLease(func() { released++ }, nil), true
	})

	assert.Equal(t, []int64{51, 52}, broadcastIDs)
	assert.Empty(t, sink.regularIDs())
	assert.Equal(t, []int64{51, 52}, sink.fromIDs())
	assert.Equal(t, 2, acquired)
	assert.Equal(t, 2, released)
}

func TestEventPublisherEventFromNilAdmissionFallsBackToOrdinarySink(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: 7})
	defer sub.Unsub()
	sink := &publisherForkSink{}
	publisher := daemon.NewEventPublisher(broadcaster, sink)

	publisher.EventFrom(7, db.Event{ID: 61, ProjectID: 7, Type: "claim.expired"}, nil)

	msg := receiveMsg(t, sub.Ch, time.Second, "published fallback event")
	require.NotNil(t, msg.Event)
	assert.Equal(t, int64(61), msg.Event.ID)
	assert.Equal(t, []int64{61}, sink.regularIDs())
	assert.Empty(t, sink.fromIDs())
}

func TestEventPublisherEventsByProjectUsesEachEventProjectID(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	projectSeven := broadcaster.Subscribe(daemon.SubFilter{ProjectID: 7})
	defer projectSeven.Unsub()
	projectEight := broadcaster.Subscribe(daemon.SubFilter{ProjectID: 8})
	defer projectEight.Unsub()
	sink := &publisherSink{}
	publisher := daemon.NewEventPublisher(broadcaster, sink)

	publisher.EventsByProject([]db.Event{
		{ID: 41, ProjectID: 7, Type: "claim.expired"},
		{ID: 42, ProjectID: 8, Type: "claim.expired"},
	})

	first := receiveMsg(t, projectSeven.Ch, time.Second, "project seven event")
	require.NotNil(t, first.Event)
	assert.Equal(t, int64(41), first.Event.ID)
	assert.Equal(t, int64(7), first.ProjectID)
	second := receiveMsg(t, projectEight.Ch, time.Second, "project eight event")
	require.NotNil(t, second.Event)
	assert.Equal(t, int64(42), second.Event.ID)
	assert.Equal(t, int64(8), second.ProjectID)
	assert.Equal(t, []int64{41, 42}, sink.ids())
}

func TestEventPublisherResetBroadcastsWithoutEnqueueing(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()
	sink := &publisherSink{}
	publisher := daemon.NewEventPublisher(broadcaster, sink)

	publisher.Reset(7, 42)

	msg := receiveMsg(t, sub.Ch, time.Second, "published reset")
	assert.Equal(t, daemon.StreamKindReset, msg.Kind)
	assert.Equal(t, int64(42), msg.ResetID)
	assert.Empty(t, sink.ids(), "reset frames are broadcast-only by design")
}

func TestEventPublisherToleratesMissingSinks(t *testing.T) {
	publisher := daemon.NewEventPublisher(nil, nil)

	assert.NotPanics(t, func() {
		publisher.Event(7, db.Event{ID: 11, ProjectID: 7, Type: "issue.created"})
		publisher.Reset(7, 42)
	}, "a publisher built with no sinks must behave like the noop wiring it replaces")
}
