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
	service.broadcaster.Broadcast(daemon.StreamMsg{
		Kind: "event", Event: &event, ProjectID: project.ID,
	})

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
