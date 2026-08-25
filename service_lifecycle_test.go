package kata

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestServiceCloseTerminatesActiveEventStream(t *testing.T) {
	service, err := New(context.Background(), Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)

	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/events/stream?after_id=0", nil)
	require.NoError(t, err)
	request.Header.Set("Accept", "text/event-stream")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)

	project, err := service.store.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	_, event, err := service.store.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "observe shutdown",
		Author:    "example-user",
	})
	require.NoError(t, err)
	service.broadcaster.Broadcast(daemon.NewEventMsg(project.ID, event))

	reader := bufio.NewReader(response.Body)
	eventSeen := make(chan error, 1)
	go func() {
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				eventSeen <- readErr
				return
			}
			if strings.TrimSpace(line) == "event: issue.created" {
				eventSeen <- nil
				return
			}
		}
	}()
	select {
	case eventErr := <-eventSeen:
		require.NoError(t, eventErr)
	case <-time.After(2 * time.Second):
		require.Fail(t, "event stream did not enter its live phase")
	}

	streamDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(io.Discard, reader)
		streamDone <- copyErr
	}()
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.Close() }()

	select {
	case closeErr := <-closeDone:
		require.NoError(t, closeErr)
	case <-time.After(2 * time.Second):
		require.Fail(t, "Close did not wait for the active event stream")
	}
	select {
	case streamErr := <-streamDone:
		require.NoError(t, streamErr)
	case <-time.After(2 * time.Second):
		require.Fail(t, "active event stream outlived Close")
	}

	postClose, err := server.Client().Get(server.URL + "/api/v1/health")
	require.NoError(t, err)
	defer func() { _ = postClose.Body.Close() }()
	assert.Equal(t, http.StatusServiceUnavailable, postClose.StatusCode)
}

func TestServiceEnsureProjectBroadcastsAndEnqueuesExactCreatedEvent(t *testing.T) {
	service, err := New(context.Background(), Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	sink := &serviceRecordingSink{}
	service.publish = daemon.NewEventPublisher(service.broadcaster, sink)
	subscription := service.broadcaster.Subscribe(daemon.SubFilter{})
	defer subscription.Unsub()

	result, err := service.EnsureProject(context.Background(), ProjectSpec{
		UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Name: "example-project",
	})
	require.NoError(t, err)
	require.True(t, result.Created)

	var msg daemon.StreamMsg
	select {
	case msg = <-subscription.Ch:
	case <-time.After(time.Second):
		t.Fatal("project creation event was not broadcast")
	}
	require.Equal(t, daemon.StreamKindEvent, msg.Kind)
	require.NotNil(t, msg.Event)
	assert.Equal(t, "project.created", msg.Event.Type)
	assert.Equal(t, db.SystemActor, msg.Event.Actor)
	require.Len(t, sink.events, 1)
	assert.Equal(t, *msg.Event, sink.events[0])
	persisted, err := service.store.EventsAfter(context.Background(), db.EventsAfterParams{
		ProjectID: result.Project.ID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, persisted, 1)
	assert.Equal(t, *msg.Event, persisted[0])
}

type serviceRecordingSink struct {
	events []db.Event
}

func (s *serviceRecordingSink) Enqueue(event db.Event) {
	s.events = append(s.events, event)
}
