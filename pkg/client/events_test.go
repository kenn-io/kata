package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventsServer serves body as a finished event stream and records the request.
func eventsServer(t *testing.T, body string) (*Client, <-chan *http.Request) {
	t.Helper()
	requests := make(chan *http.Request, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	return api, requests
}

// collectEvents drains the iterator and returns the events plus the error that
// ended it.
func collectEvents(t *testing.T, api *Client, opts EventsOptions) ([]Event, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var events []Event
	for event, err := range api.Events(ctx, opts) {
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
	return events, nil
}

func TestEventsYieldsFramedEventsThenReportsStreamEnd(t *testing.T) {
	api, _ := eventsServer(t, ": keepalive\n\n"+
		"id: 1\nevent: issue.created\ndata: {\"type\":\"issue.created\"}\n\n"+
		"id: 2\r\nevent: issue.commented\r\ndata: {\"type\":\"issue.commented\"}\r\n\r\n"+
		"id: 3\nevent: issue.updated\n\n"+ // no data: dropped
		"id: 4\nevent: issue.closed\ndata:{\"type\":\"issue.closed\"}\n\n"+ // no space after the colon
		"id: 5\nevent: issue.reopened\ndata: {\"type\":\"issue.reopened\"}\n") // no blank line: dropped

	events, err := collectEvents(t, api, EventsOptions{})

	require.ErrorIs(t, err, ErrEventStreamEnded)
	require.Len(t, events, 3)
	assert.Equal(t, Event{ID: 1, Type: "issue.created", Data: []byte(`{"type":"issue.created"}`)}, events[0])
	assert.Equal(t, int64(2), events[1].ID)
	assert.Equal(t, "issue.commented", events[1].Type)
	assert.Equal(t, int64(4), events[2].ID)
	assert.JSONEq(t, `{"type":"issue.closed"}`, string(events[2].Data))
}

func TestEventsJoinsMultilineData(t *testing.T) {
	api, _ := eventsServer(t, "id: 7\nevent: issue.created\ndata: {\"a\":\ndata: 1}\n\n")

	events, _ := collectEvents(t, api, EventsOptions{})

	require.Len(t, events, 1)
	assert.Equal(t, "{\"a\":\n1}", string(events[0].Data))
}

func TestEventsSurfacesResetRequiredAsTypedValue(t *testing.T) {
	api, _ := eventsServer(t, "id: 42\nevent: sync.reset_required\ndata: {\"event_id\":42,\"reset_after_id\":40}\n\n")

	events, _ := collectEvents(t, api, EventsOptions{})

	require.Len(t, events, 1)
	require.NotNil(t, events[0].Reset)
	assert.Equal(t, ResetRequired{EventID: 42, ResetAfterID: 40}, *events[0].Reset)
	assert.Equal(t, int64(42), events[0].ID, "the resume cursor stays the frame id")
	assert.Equal(t, "sync.reset_required", events[0].Type)
}

func TestEventsResetWithUnreadablePayloadFallsBackToFrameID(t *testing.T) {
	api, _ := eventsServer(t, "id: 42\nevent: sync.reset_required\ndata: not-json\n\n")

	events, _ := collectEvents(t, api, EventsOptions{})

	require.Len(t, events, 1)
	require.NotNil(t, events[0].Reset)
	assert.Equal(t, int64(42), events[0].Reset.ResetAfterID)
}

func TestEventsSendsCursorAndProjectScope(t *testing.T) {
	api, requests := eventsServer(t, "")
	projectID := int64(7)

	_, _ = collectEvents(t, api, EventsOptions{ProjectID: &projectID, LastEventID: 9})

	req := <-requests
	assert.Equal(t, "9", req.Header.Get("Last-Event-ID"))
	assert.Equal(t, "7", req.URL.Query().Get("project_id"))
	assert.Equal(t, "text/event-stream", req.Header.Get("Accept"))
}

func TestEventsOmitsCursorAndScopeByDefault(t *testing.T) {
	api, requests := eventsServer(t, "")

	_, _ = collectEvents(t, api, EventsOptions{})

	req := <-requests
	assert.Empty(t, req.Header.Get("Last-Event-ID"))
	assert.False(t, req.URL.Query().Has("project_id"))
}

func TestEventsRejectsAnEventOverTheSizeCap(t *testing.T) {
	big := strings.Repeat("x", 200)
	api, _ := eventsServer(t, "id: 1\nevent: a\ndata: ok\n\n"+"id: 2\nevent: b\ndata: "+big+"\n\n")

	events, err := collectEvents(t, api, EventsOptions{MaxEventBytes: 100})

	require.ErrorIs(t, err, ErrEventTooLarge)
	require.Len(t, events, 1, "the event before the oversized one still arrives")
	assert.Equal(t, int64(1), events[0].ID)
}

func TestEventsSizeCapCoversManyDataLines(t *testing.T) {
	var body strings.Builder
	body.WriteString("id: 1\nevent: a\n")
	for range 50 {
		body.WriteString("data: 0123456789\n")
	}
	body.WriteString("\n")
	api, _ := eventsServer(t, body.String())

	_, err := collectEvents(t, api, EventsOptions{MaxEventBytes: 100})

	require.ErrorIs(t, err, ErrEventTooLarge)
}

func TestEventsSizeCapDoesNotBoundTheWholeStream(t *testing.T) {
	var body strings.Builder
	for i := range 20 {
		body.WriteString("id: " + string(rune('1'+i%9)) + "\nevent: a\ndata: 0123456789\n\n")
	}
	api, _ := eventsServer(t, body.String())

	events, err := collectEvents(t, api, EventsOptions{MaxEventBytes: 100})

	require.ErrorIs(t, err, ErrEventStreamEnded)
	assert.Len(t, events, 20)
}

func TestEventsStopsWhenTheCallerBreaks(t *testing.T) {
	api, _ := eventsServer(t, "id: 1\nevent: a\ndata: 1\n\nid: 2\nevent: a\ndata: 2\n\n")
	var seen []int64

	for event, err := range api.Events(t.Context(), EventsOptions{}) {
		require.NoError(t, err)
		seen = append(seen, event.ID)
		break
	}

	assert.Equal(t, []int64{1}, seen)
}

func TestEventsCancellationEndsALiveStream(t *testing.T) {
	flushed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\nevent: a\ndata: 1\n\n")
		w.(http.Flusher).Flush()
		close(flushed)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var seen int
	var last error
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event, err := range api.Events(ctx, EventsOptions{}) {
			if err != nil {
				last = err
				return
			}
			seen++
			assert.Equal(t, int64(1), event.ID)
			<-flushed
			cancel()
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("iterator did not stop after cancellation")
	}
	assert.Equal(t, 1, seen)
	require.ErrorIs(t, last, context.Canceled)
}

func TestEventsReportsAnHTTPErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)

	_, err = collectEvents(t, api, EventsOptions{})

	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, StatusCode(err))
}
