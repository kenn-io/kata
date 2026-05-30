package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// sseClient is the subset of *http.Client startSSE needs. Defining it
// as an interface lets sse_test.go drive readSSEStream against an
// httptest.Server-built client without exposing http.Client internals.
type sseClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// reconnectStatusGrace defers surfacing sseReconnecting to the UI for
// this long after a disconnect. Brief outages (daemon restarts, transient
// network blips) usually recover inside the window so the user never sees
// the badge. The grace runs in parallel with the reconnect loop — it does
// not delay the reconnect attempt itself, only the user-visible signal.
//
// var (not const) so tests can shorten it.
var reconnectStatusGrace = 1500 * time.Millisecond

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
	ctx context.Context, hc sseClient, base string, projectID *int64, sseCh chan<- tea.Msg,
) {
	startSSEForConnection(ctx, hc, base, projectID, sseCh, 0)
}

func startSSEForConnection(
	ctx context.Context, hc sseClient, base string, projectID *int64, sseCh chan<- tea.Msg, gen uint64,
) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
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

	for {
		if ctx.Err() != nil {
			return
		}
		connected, err := readSSEStream(
			ctx, hc, base, projectID, lastID, sseCh, &lastID, publishConnected, gen,
		)
		if err == nil || ctx.Err() != nil {
			return
		}
		armGrace()
		if connected {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// nextBackoff doubles d but caps at ceiling so the goroutine doesn't
// spin at 1s forever yet doesn't sleep longer than the reconnect cap.
func nextBackoff(d, ceiling time.Duration) time.Duration {
	if d >= ceiling {
		return ceiling
	}
	d *= 2
	if d > ceiling {
		d = ceiling
	}
	return d
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

// readSSEStream issues GET /api/v1/events/stream and consumes frames
// until disconnect. lastID rides Last-Event-ID for resume when >0.
// connected is true once at least one frame was emitted so callers
// reset their backoff only on a productive connection.
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
	ctx context.Context, hc sseClient, base string, projectID *int64,
	lastID int64, sseCh chan<- tea.Msg, updateLastID *int64, onConnect func(), gen ...uint64,
) (bool, error) {
	req, err := buildSSERequest(ctx, base, projectID, lastID)
	if err != nil {
		return false, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("sse: status %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	connected := false
	for {
		f, perr := readNextFrame(br)
		if errors.Is(perr, errSSEEOF) {
			return connected, errSSEEOF
		}
		if perr != nil {
			return connected, perr
		}
		if !connected {
			if onConnect != nil {
				onConnect()
			} else {
				notifyStatus(ctx, sseCh, sseConnected, firstGen(gen))
			}
			connected = true
		}
		*updateLastID = f.id
		if !forwardFrame(ctx, sseCh, f, firstGen(gen)) {
			return connected, ctx.Err()
		}
	}
}

func firstGen(gen []uint64) uint64 {
	if len(gen) == 0 {
		return 0
	}
	return gen[0]
}

// buildSSERequest composes the streaming request. project_id is omitted
// in all-projects mode; Last-Event-ID is omitted on first connect.
func buildSSERequest(
	ctx context.Context, base string, projectID *int64, lastID int64,
) (*http.Request, error) {
	url := base + "/api/v1/events/stream"
	if projectID != nil {
		url += "?project_id=" + strconv.FormatInt(*projectID, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastID, 10))
	}
	return req, nil
}

// forwardFrame dispatches the parsed frame as the matching tea.Msg.
// Returns false on ctx cancel so the caller exits the read loop.
//
// The reset_after_id payload field is not threaded onto resetRequiredMsg:
// the daemon's contract (internal/api/events.go EventReset.EventID ==
// ResetAfterID) makes the SSE id: line — which already feeds Last-Event-ID
// — the authoritative resume checkpoint.
func forwardFrame(ctx context.Context, sseCh chan<- tea.Msg, f frame, gen ...uint64) bool {
	var msg tea.Msg
	if f.kind == frameReset {
		msg = resetRequiredMsg{gen: firstGen(gen)}
	} else {
		ev := decodeEventReceived(f)
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
