package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"strconv"

	"go.kenn.io/kata/pkg/client/generated"
)

// maxEventBytes caps one frame, comments and field names included. The daemon
// does not bound an event below any cap (imports take bodies up to 64 MiB), so
// a frame over it is skipped and reported as Event.Oversized, never as a
// failure of the whole stream.
//
// var (not const) so tests can lower it.
var maxEventBytes = 8 << 20

// resetRequiredType is the SSE event name of a cursor the daemon can no longer
// resume from.
const resetRequiredType = "sync.reset_required"

var (
	// ErrEventStreamEnded ends Events when the daemon closes the stream. The
	// last Event.ID is the cursor to reconnect from.
	ErrEventStreamEnded = errors.New("event stream ended")
)

// EventsOptions selects what Events streams.
type EventsOptions struct {
	// ProjectID limits the stream to one project. Nil streams every project the
	// caller can see.
	ProjectID *int64
	// LastEventID resumes after that event id, sent as Last-Event-ID. Zero
	// starts from the live end of the stream.
	LastEventID int64
}

// Event is one frame of the event stream.
type Event struct {
	// ID is the frame's id: line, the cursor to resume from. Zero when the
	// frame carried none.
	ID int64
	// Type is the frame's event: line, such as "issue.created".
	Type string
	// Data is the frame's data: lines joined with newlines, normally one JSON
	// object. A reset frame also keeps its raw payload here.
	Data []byte
	// Reset is set only for sync.reset_required: the cursor fell behind a purge,
	// so the caller must drop its cache and resume from Reset.ResetAfterID.
	Reset *ResetRequired
	// Oversized is set when the frame passed the size cap and was skipped. Only
	// ID is filled in; the caller cannot read the event, so it should refetch
	// what it caches and resume after ID.
	Oversized bool
}

// ResetRequired is the payload of a sync.reset_required frame.
type ResetRequired struct {
	ResetAfterID int64 `json:"reset_after_id"`
}

// Events follows the daemon's event stream over one connection and yields each
// frame as it arrives. Keepalive comments and frames without both an event
// type and data are skipped. Break out of the loop, or cancel ctx, to close the
// connection.
//
// A failure to connect, a read error, or the daemon closing the stream is
// yielded as the final error; the zero Event comes with
// it. Events does not reconnect: pass the last Event.ID as LastEventID to
// resume. A frame over the size cap is skipped and yielded as an Oversized
// event. After a reset frame the daemon ends the stream, and the next
// connection should resume from Reset.ResetAfterID.
func (c *Client) Events(ctx context.Context, opts EventsOptions) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		query := &generated.StreamEventsQuery{ProjectID: opts.ProjectID}
		resp, err := c.StreamEventsRaw(ctx, &generated.StreamEventsRequestOptions{Query: query},
			func(_ context.Context, req *http.Request) error {
				if opts.LastEventID > 0 {
					req.Header.Set("Last-Event-ID", strconv.FormatInt(opts.LastEventID, 10))
				}
				return nil
			})
		if err != nil {
			yield(Event{}, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		reader := frameReader{br: bufio.NewReader(resp.Body)}
		for {
			event, err := reader.next()
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					err = ctxErr
				}
				yield(Event{}, err)
				return
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

type frameReader struct {
	br *bufio.Reader
}

// next reads up to the next complete frame. A frame cut off by the end of the
// stream is dropped.
func (r *frameReader) next() (Event, error) {
	var (
		event            Event
		hasType, hasData bool
		data             []byte
		size             int
		oversized        bool
	)
	for {
		n, line, err := r.readLine(&size)
		if err != nil {
			return Event{}, err
		}
		if n > 0 && line == nil {
			oversized = true
			continue
		}
		if n == 0 {
			if oversized {
				return Event{ID: event.ID, Oversized: true}, nil
			}
			if !hasType || !hasData {
				event, hasType, hasData, data, size = Event{}, false, false, nil, 0
				continue
			}
			event.Data = data
			if event.Type == resetRequiredType {
				event.Reset = decodeReset(event)
				if event.ID == 0 {
					event.ID = event.Reset.ResetAfterID
				}
			}
			return event, nil
		}
		if line[0] == ':' || oversized {
			continue
		}
		name, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(name) {
		case "id":
			if n, err := strconv.ParseInt(string(value), 10, 64); err == nil {
				event.ID = n
			}
		case "event":
			if len(value) > 0 {
				event.Type = string(value)
				hasType = true
			}
		case "data":
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
		}
	}
}

// readLine returns one line without its terminator and its length, adding the
// bytes read to *size. Once they pass the cap the line is consumed but not
// kept: the returned slice is nil while the length stays positive.
func (r *frameReader) readLine(size *int) (int, []byte, error) {
	var (
		line []byte
		n    int
	)
	for {
		chunk, more, err := r.br.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, nil, ErrEventStreamEnded
			}
			return 0, nil, err
		}
		n += len(chunk)
		*size += len(chunk) + 1
		if *size > maxEventBytes {
			line = nil
		} else {
			line = append(line, chunk...)
		}
		if !more {
			return n, line, nil
		}
	}
}

// decodeReset reads a reset frame's payload. The daemon makes the frame's id:
// line equal reset_after_id, so a payload that does not decode falls back to it.
func decodeReset(event Event) *ResetRequired {
	var reset ResetRequired
	if err := json.Unmarshal(event.Data, &reset); err != nil {
		return &ResetRequired{ResetAfterID: event.ID}
	}
	return &reset
}
