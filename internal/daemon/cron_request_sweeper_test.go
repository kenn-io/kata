package daemon_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type requestSweepStore struct {
	db.Storage
	event     db.Event
	published *captureHookSink
	clocks    []time.Time
}

func (s *requestSweepStore) ListCronRequestProjects(context.Context) ([]int64, error) {
	return []int64{101, 102, 103}, nil
}
func (s *requestSweepStore) SweepCronRequests(_ context.Context, pid int64, now time.Time) (*db.Event, error) {
	s.clocks = append(s.clocks, now)
	switch pid {
	case 101:
		return &s.event, errors.New("malformed key retained beside committed expiry")
	case 102:
		if len(s.published.events) != 1 {
			return nil, errors.New("first commit not published before next project")
		}
		return nil, errors.New("project failed")
	default:
		return nil, nil
	}
}

// Every committed expiry event must reach clients and hooks immediately, even
// when that project reports malformed retained data or another project fails.
func TestCronRequestSweepPublishesBeforeNextProject(t *testing.T) {
	env := testenv.New(t)
	sink := &captureHookSink{}
	event := db.Event{ID: 1001, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectID: 101, Type: "project.metadata_updated", Actor: "system"}
	store := &requestSweepStore{Storage: env.DB, event: event, published: sink}
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: 101})
	defer sub.Unsub()
	now := time.Now().UTC()
	err := daemon.NewDueNotificationSweeper(store, daemon.NewEventPublisher(broadcaster, sink), "UTC").RunOnce(t.Context(), now)
	require.ErrorContains(t, err, "malformed key")
	require.ErrorContains(t, err, "project failed")
	require.NotContains(t, err.Error(), "not published")
	require.Equal(t, []time.Time{now, now, now}, store.clocks)
	require.Equal(t, []db.Event{event}, sink.events)
	require.Equal(t, event.ID, receiveMsg(t, sub.Ch, time.Second, "request expiry broadcast").Event.ID)
}
