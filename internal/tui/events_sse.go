package tui

import (
	"context"
	"net/http"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/cenkalti/backoff/v7"
	kataclient "go.kenn.io/kata/pkg/client"
)

// reconnectStatusGrace defers surfacing sseReconnecting to the UI for
// this long after a disconnect. Brief outages (daemon restarts, transient
// network blips) usually recover inside the window so the user never sees
// the badge. The grace runs in parallel with the reconnect loop — it does
// not delay the reconnect attempt itself, only the user-visible signal.
//
// var (not const) so tests can shorten it.
var reconnectStatusGrace = 1500 * time.Millisecond

// initialReconnectBackoff is the first reconnect delay after a disconnect.
//
// var (not const) so tests can shorten it.
var initialReconnectBackoff = time.Second

// startSSE is the long-lived consumer goroutine. Loops over
// readSSEStream, reconnects with exponential backoff (1s → 30s, capped),
// and resumes via Last-Event-ID once at least one frame was emitted on
// the prior connection.
//
// We deliberately do NOT push sseConnected before readSSEStream returns
// its first frame — readSSEStream emits sseConnected itself once a frame
// arrives. Pushing optimistically here would flicker connected ↔
// reconnecting on a flapping daemon: the user would see "connected" the
// moment we issue the request, "reconnecting" on the inevitable error,
// and so on per loop turn even though no frame ever made it through.
//
// sseReconnecting is debounced by reconnectStatusGrace: armed on the
// first disconnect of an outage, fired only if the reconnect hasn't
// produced a frame within the grace window, and cancelled when the next
// successful read produces its first frame (or on goroutine exit).
//
// publishConnected and the AfterFunc callback share a mutex so the two
// state transitions cannot interleave: once a callback observes
// "connected" it returns without sending; once publishConnected sets
// "connected" any later or in-flight callback observes it and bails.
// When the callback wins the race it sends sseReconnecting *before*
// publishConnected sends sseConnected, so the channel ordering keeps
// the final state correct (connected wins, reconnecting was a brief
// flash that the consumer overwrites).
func startSSE(
	ctx context.Context, hc *http.Client, base string, projectID *int64, sseCh chan<- tea.Msg,
) {
	startSSEForConnection(ctx, hc, base, projectID, sseCh, 0)
}

func startSSEForConnection(
	ctx context.Context, hc *http.Client, base string, projectID *int64, sseCh chan<- tea.Msg, gen uint64,
) {
	const maxBackoff = 30 * time.Second
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = initialReconnectBackoff
	bo.MaxInterval = maxBackoff
	bo.Multiplier = 2
	var lastID int64

	var (
		stateMu      sync.Mutex
		graceTimer   *time.Timer // nil when no grace window is pending
		hasConnected bool        // true once sseConnected has been sent for the current outage
		inOutage     bool
	)

	publishConnected := func() {
		stateMu.Lock()
		defer stateMu.Unlock()
		if graceTimer != nil {
			graceTimer.Stop()
			graceTimer = nil
		}
		inOutage = false
		hasConnected = true
		notifyStatus(ctx, sseCh, sseConnected, gen)
	}
	armGrace := func() {
		stateMu.Lock()
		defer stateMu.Unlock()
		if inOutage {
			return
		}
		inOutage = true
		hasConnected = false
		graceTimer = time.AfterFunc(reconnectStatusGrace, func() {
			stateMu.Lock()
			defer stateMu.Unlock()
			if hasConnected {
				return
			}
			notifyStatus(ctx, sseCh, sseReconnecting, gen)
		})
	}
	defer func() {
		stateMu.Lock()
		if graceTimer != nil {
			graceTimer.Stop()
			graceTimer = nil
		}
		stateMu.Unlock()
	}()

	_, _ = backoff.Retry(ctx, func() (struct{}, error) {
		if ctx.Err() != nil {
			return struct{}{}, nil
		}
		connected, err := readSSEStream(ctx, hc, base, projectID, lastID, sseCh, &lastID, publishConnected, gen)
		if err == nil || ctx.Err() != nil {
			return struct{}{}, nil
		}
		armGrace()
		if connected {
			bo.Reset()
		}
		return struct{}{}, err
	}, backoff.WithBackOff(bo), backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0))
}

// notifyStatus pushes an sseStatusMsg without blocking past ctx cancel.
func notifyStatus(ctx context.Context, sseCh chan<- tea.Msg, st sseConnState, gen ...uint64) {
	var g uint64
	if len(gen) > 0 {
		g = gen[0]
	}
	select {
	case sseCh <- sseStatusMsg{gen: g, state: st}:
	case <-ctx.Done():
	}
}

// readSSEStream follows /api/v1/events/stream through the client's Events
// iterator until disconnect. lastID rides Last-Event-ID for resume when >0.
// connected is true once at least one frame was emitted so callers
// reset their backoff only on a productive connection. A stream the
// daemon closes returns kataclient.ErrEventStreamEnded.
//
// sseConnected is pushed exactly once per connection, on the first
// successful frame — never optimistically before a frame lands. A
// connection that fails before any frame arrives produces no connected
// status, only the reconnecting status the caller emits on disconnect.
//
// onConnect, when non-nil, is invoked on the first-frame transition and
// owns the sseConnected publication (it is responsible for serializing
// the send with any concurrent reconnect-status timer). When onConnect
// is nil readSSEStream emits sseConnected directly — the path tests
// take when driving readSSEStream without the startSSE wrapper.
func readSSEStream(
	ctx context.Context, hc *http.Client, base string, projectID *int64,
	lastID int64, sseCh chan<- tea.Msg, updateLastID *int64, onConnect func(), gen ...uint64,
) (bool, error) {
	apiClient, err := kataclient.NewWithHTTPClient(base, hc)
	if err != nil {
		return false, err
	}
	connected := false
	for e, err := range apiClient.Events(ctx, kataclient.EventsOptions{ProjectID: projectID, LastEventID: lastID}) {
		if err != nil {
			return connected, err
		}
		if !connected {
			if onConnect != nil {
				onConnect()
			} else {
				notifyStatus(ctx, sseCh, sseConnected, firstGen(gen))
			}
			connected = true
		}
		*updateLastID = e.ID
		if !forwardFrame(ctx, sseCh, e, firstGen(gen)) {
			return connected, ctx.Err()
		}
	}
	return connected, nil
}

func firstGen(gen []uint64) uint64 {
	if len(gen) == 0 {
		return 0
	}
	return gen[0]
}

// forwardFrame dispatches the event as the matching tea.Msg.
// Returns false on ctx cancel so the caller exits the read loop.
//
// A reset and an oversized frame (one the client skipped for passing its size
// cap) both become resetRequiredMsg: the cache is refetched and the stream
// resumes after the event's ID, which already feeds Last-Event-ID. The reset's
// ResetAfterID is not threaded onto the message: the daemon's contract
// (internal/api/events.go EventReset.EventID == ResetAfterID) makes the event
// ID the authoritative resume checkpoint.
func forwardFrame(ctx context.Context, sseCh chan<- tea.Msg, event kataclient.Event, gen ...uint64) bool {
	var msg tea.Msg
	if event.Reset != nil || event.Oversized {
		msg = resetRequiredMsg{gen: firstGen(gen)}
	} else {
		ev := decodeEventReceived(event)
		ev.gen = firstGen(gen)
		msg = ev
	}
	select {
	case sseCh <- msg:
		return true
	case <-ctx.Done():
		return false
	}
}
