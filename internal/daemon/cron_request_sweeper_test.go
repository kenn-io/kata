package daemon_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

type requestSweepStore struct {
	db.Storage
	events    []db.Event
	published *captureHookSink
	calls     int
	clocks    []time.Time
}

func (s *requestSweepStore) ListCronRequestProjects(context.Context) ([]int64, error) {
	return []int64{101, 102, 103}, nil
}
func (s *requestSweepStore) SweepCronRequests(_ context.Context, pid int64, now time.Time) (*db.Event, error) {
	s.calls++
	s.clocks = append(s.clocks, now)
	if pid == 102 {
		requirePublished := len(s.published.events) == 1
		if !requirePublished {
			return nil, errors.New("first commit not published before next project")
		}
		return nil, errors.New("project failed")
	}
	if pid == 103 {
		return nil, nil
	}
	return &s.events[0], errors.New("malformed key retained beside committed expiry")
}

// Every committed expiry event must reach clients and hooks immediately, even
// when that project reports malformed retained data or another project fails.
func TestCronRequestSweepPublishesBeforeNextProject(t *testing.T) {
	env := testenv.New(t)
	sink := &captureHookSink{}
	event := db.Event{ID: 1001, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectID: 101, Type: "project.metadata_updated", Actor: "system"}
	store := &requestSweepStore{Storage: env.DB, events: []db.Event{event}, published: sink}
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: 101})
	defer sub.Unsub()
	sweeper := daemon.NewDueNotificationSweeper(store, daemon.NewEventPublisher(broadcaster, sink), "UTC")
	now := time.Now().UTC()
	err := sweeper.RunOnce(t.Context(), now)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "not published")
	require.Contains(t, err.Error(), "malformed key")
	require.Contains(t, err.Error(), "project failed")
	require.Equal(t, 3, store.calls)
	require.Equal(t, []time.Time{now, now, now}, store.clocks)
	require.Equal(t, []db.Event{event}, sink.events)
	msg := receiveMsg(t, sub.Ch, time.Second, "request expiry broadcast")
	require.Equal(t, event.ID, msg.Event.ID)
}

func TestCronRequestSweepPublishesCommittedMetadata(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	p, err := env.DB.CreateProject(ctx, "expiry-project")
	require.NoError(t, err)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id, err := uid.FromTime(now)
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{"cron.request." + id: map[string]any{"kind": "cancel", "run_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "requested_by": "operator", "requested_at": now, "expires_at": now.Add(time.Minute)}})
	require.NoError(t, err)
	_, err = env.DB.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(raw), p.ID)
	require.NoError(t, err)
	sink := &captureHookSink{}
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	sweeper := daemon.NewDueNotificationSweeper(env.DB, daemon.NewEventPublisher(broadcaster, sink), "UTC")
	require.NoError(t, sweeper.RunOnce(ctx, now.Add(time.Minute)))
	msg := receiveMsg(t, sub.Ch, time.Second, "expiry metadata broadcast")
	require.NotNil(t, msg.Event)
	durable, err := env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, AfterID: msg.Event.ID - 1, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []db.Event{*msg.Event}, durable, "publish the exact committed event")
	require.Len(t, sink.events, 1)
	require.Equal(t, msg.Event.UID, sink.events[0].UID)
	p, err = env.DB.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(p.Metadata))
	require.NoError(t, sweeper.RunOnce(ctx, now.Add(time.Hour)))
	require.Len(t, sink.events, 1)
}
