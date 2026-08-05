package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
)

func TestFinishProjectInitDeliversBeforeConfigPersistence(t *testing.T) {
	broadcaster := NewEventBroadcaster()
	subscription := broadcaster.Subscribe(SubFilter{})
	defer subscription.Unsub()
	event := &db.Event{ID: 11, ProjectID: 7, Type: "project.created", Actor: "user-a"}
	configWriteCalled := false

	err := finishProjectInit(ServerConfig{
		Broadcaster: broadcaster,
		Hooks:       hooks.NewNoop(),
	}, projectInitMutation{Event: event, ProjectID: event.ProjectID, writeConfig: func() error {
		configWriteCalled = true
		select {
		case message := <-subscription.Ch:
			require.Equal(t, "event", message.Kind)
			assert.Equal(t, event, message.Event)
		default:
			t.Fatal("project event was not delivered before config persistence")
		}
		return errors.New("config write failed")
	}}, nil)

	require.Error(t, err)
	assert.True(t, configWriteCalled)
}
