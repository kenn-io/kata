package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/testenv"
)

func TestDueNotificationSweeperPublishesOrdinaryMetadataEvent(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "scheduled-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Scheduled work", Author: "coordinator",
		Metadata: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-09-16T09:00"`),
		},
	})
	require.NoError(t, err)

	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	hookSink := &captureHookSink{}
	sweeper := daemon.NewDueNotificationSweeper(
		env.DB, daemon.NewEventPublisher(broadcaster, hookSink), "UTC",
	)

	require.NoError(t, sweeper.RunOnce(ctx,
		time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)))
	msg := receiveMsg(t, sub.Ch, time.Second, "due notification broadcast")
	require.NotNil(t, msg.Event)
	assert.Equal(t, "issue.metadata_updated", msg.Event.Type)
	assert.Equal(t, "system", msg.Event.Actor)
	require.Len(t, hookSink.events, 1)
	assert.Equal(t, msg.Event.ID, hookSink.events[0].ID)

	got, err := env.DB.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	var values map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(got.Metadata), &values))
	assert.JSONEq(t,
		`{"from":"system","message":"Scheduled date reached: 2026-09-16T09:00"}`,
		string(values[notification.MetadataKey("coordinator")]),
	)

	require.NoError(t, sweeper.RunOnce(ctx,
		time.Date(2026, 9, 16, 10, 1, 0, 0, time.UTC)))
	assertNoReceive(t, sub.Ch, 100*time.Millisecond, "delivered date must not repeat")
	require.Len(t, hookSink.events, 1)
}

func TestDueNotificationSweeperContinuesAfterInvalidRecipient(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "recipient-project")
	require.NoError(t, err)
	due := map[string]json.RawMessage{"deadline_on": json.RawMessage(`"2026-09-16"`)}
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Invalid recipient", Author: "bad\nactor", Metadata: due,
	})
	require.NoError(t, err)
	valid, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Valid recipient", Author: "coordinator", Metadata: due,
	})
	require.NoError(t, err)

	broadcaster := daemon.NewEventBroadcaster()
	hookSink := &captureHookSink{}
	sweeper := daemon.NewDueNotificationSweeper(
		env.DB, daemon.NewEventPublisher(broadcaster, hookSink), "UTC",
	)
	err = sweeper.RunOnce(ctx, time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC))
	require.Error(t, err)
	require.Len(t, hookSink.events, 1)
	require.NotNil(t, hookSink.events[0].IssueID)
	assert.Equal(t, valid.ID, *hookSink.events[0].IssueID)
}
