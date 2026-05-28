package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

const (
	pollLimitDefault = 100
	pollLimitMax     = 1000

	// sseDrainCap is the max number of events the drain phase replays. Spec §4.8
	// says "bounded ~10k rows"; we query LIMIT cap+1 so we can detect "too far
	// behind" and emit sync.reset_required instead.
	sseDrainCap = 10000
	// sseLiveBatch caps each live-phase re-query at this many rows. A single
	// wakeup typically returns 1; we still cap to avoid pathological cases.
	sseLiveBatch = 1000

	// heartbeatInterval is the SSE keepalive period. Comments are no-ops per the
	// SSE spec; their purpose is to keep TCP connections alive through middleboxes.
	heartbeatInterval = 25 * time.Second
)

func registerEventsHandlers(humaAPI huma.API, mux *http.ServeMux, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "pollEvents",
		Method:      "GET",
		Path:        "/api/v1/events",
	}, func(ctx context.Context, in *api.PollEventsGlobalRequest) (*api.PollEventsResponse, error) {
		return doPollEvents(ctx, cfg, in.AfterID, in.Limit, 0)
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "pollProjectEvents",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/events",
	}, func(ctx context.Context, in *api.PollEventsRequest) (*api.PollEventsResponse, error) {
		if in.ProjectID <= 0 {
			return nil, api.NewError(400, "validation", "project_id must be a positive integer", "", nil)
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		return doPollEvents(ctx, cfg, in.AfterID, in.Limit, in.ProjectID)
	})

	// SSE: not Huma — needs a streaming http.HandlerFunc on the raw mux.
	mux.HandleFunc("/api/v1/events/stream", sseHandler(cfg))
}

// resolveLimit normalizes the optional Limit query param: explicit non-positive
// values are a 400 validation error; missing or zero values default to
// pollLimitDefault; values above pollLimitMax silently clamp.
func resolveLimit(rawLimit api.OptionalInt) (int, error) {
	if rawLimit.IsSet && rawLimit.Value <= 0 {
		return 0, api.NewError(400, "validation", "limit must be a positive integer", "", nil)
	}
	if !rawLimit.IsSet {
		return pollLimitDefault, nil
	}
	if rawLimit.Value > pollLimitMax {
		return pollLimitMax, nil
	}
	return rawLimit.Value, nil
}

// doPollEvents is the shared implementation for both polling endpoints. When
// projectID is 0 it is a cross-project poll; otherwise events are filtered to
// that project.
func doPollEvents(
	ctx context.Context,
	cfg ServerConfig,
	afterID int64,
	rawLimit api.OptionalInt,
	projectID int64,
) (*api.PollEventsResponse, error) {
	if afterID < 0 {
		return nil, api.NewError(400, "validation",
			"after_id must be a non-negative integer", "", nil)
	}
	limit, err := resolveLimit(rawLimit)
	if err != nil {
		return nil, err
	}

	resetTo, err := cfg.DB.PurgeResetCheck(ctx, afterID, projectID)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if resetTo > 0 {
		out := &api.PollEventsResponse{}
		out.Body.ResetRequired = true
		out.Body.ResetAfterID = resetTo
		out.Body.Events = []api.EventEnvelope{}
		out.Body.NextAfterID = resetTo
		return out, nil
	}

	rows, err := cfg.DB.EventsAfter(ctx, db.EventsAfterParams{
		AfterID:   afterID,
		ProjectID: projectID,
		Limit:     limit,
	})
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}

	out := &api.PollEventsResponse{}
	out.Body.ResetRequired = false
	out.Body.Events = toEnvelopes(rows)
	out.Body.NextAfterID = nextAfterID(rows, afterID)
	return out, nil
}

func toEnvelopes(rows []db.Event) []api.EventEnvelope {
	out := make([]api.EventEnvelope, 0, len(rows))
	for _, r := range rows {
		out = append(out, eventToEnvelope(r))
	}
	return out
}

func eventToEnvelope(e db.Event) api.EventEnvelope {
	var payload json.RawMessage
	if e.Payload != "" {
		payload = json.RawMessage(e.Payload)
	}
	return api.EventEnvelope{
		EventID:           e.ID,
		EventUID:          e.UID,
		OriginInstanceUID: e.OriginInstanceUID,
		Type:              e.Type,
		ProjectID:         e.ProjectID,
		ProjectUID:        e.ProjectUID,
		ProjectName:       e.ProjectName,
		IssueID:           e.IssueID,
		IssueUID:          e.IssueUID,
		// IssueShortID / RelatedIssueShortID are joined from issues.short_id
		// at query time so old events render correctly across short_id
		// shifts (project merge, federation merge). UIDs remain canonical.
		IssueShortID:        e.IssueShortID,
		RelatedIssueID:      e.RelatedIssueID,
		RelatedIssueUID:     e.RelatedIssueUID,
		RelatedIssueShortID: e.RelatedIssueShortID,
		Actor:               e.Actor,
		HLCPhysicalMS:       e.HLCPhysicalMS,
		HLCCounter:          e.HLCCounter,
		ContentHash:         e.ContentHash,
		Payload:             payload,
		CreatedAt:           e.CreatedAt,
	}
}

func nextAfterID(rows []db.Event, afterID int64) int64 {
	if len(rows) == 0 {
		return afterID
	}
	return rows[len(rows)-1].ID
}

// sseHandler implements GET /api/v1/events/stream.
//
// Order of operations: (1) Accept negotiation — 406 on miss/wrong;
// (2) cursor parse — 400 cursor_conflict if both header and ?after_id set;
// (3) write SSE handshake bytes and flush; (4) subscribe to broadcaster;
// (5) capture hwm = MaxEventID; (6) PurgeResetCheck — if hit, write reset
// frame and return; (7) drain events (cursor, hwm] up to sseDrainCap+1;
// (8) if drain hit cap+1, emit reset frame at hwm and return (stale-cap);
// (9) write drained frames in id order; (10) live phase (Task 7).
//
// Steps 4–6 are Subscribe-first / check-second so a purge that fires between
// cursor parse and Subscribe lands on sub.Ch via the live channel; one
// committed before parse is captured by PurgeResetCheck. See spec §5.3.
func sseHandler(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			api.WriteEnvelope(w, http.StatusMethodNotAllowed, "method_not_allowed",
				"events stream only accepts GET")
			return
		}
		if !acceptableForSSE(r.Header.Get("Accept")) {
			api.WriteEnvelope(w, http.StatusNotAcceptable, "not_acceptable",
				"Accept must be text/event-stream")
			return
		}

		// cursor_conflict is checked on header/query *key presence* before
		// parsing values. A request with `Last-Event-ID: 5` plus `?after_id=`
		// (empty) or `?after_id=&after_id=5` (multi-value) would otherwise
		// bypass detection if we asked Get(), which only returns the first
		// non-empty value.
		_, hadHeader := r.Header[http.CanonicalHeaderKey("Last-Event-ID")]
		_, hadQuery := r.URL.Query()["after_id"]
		if hadHeader && hadQuery {
			renderAPIError(w, api.NewError(400, "cursor_conflict",
				"pass either Last-Event-ID or ?after_id, not both", "", nil))
			return
		}
		cursor, perr := parseSSECursor(r)
		if perr != nil {
			renderAPIError(w, perr)
			return
		}

		var projectID int64
		if pidStr := r.URL.Query().Get("project_id"); pidStr != "" {
			n, err := strconv.ParseInt(pidStr, 10, 64)
			if err != nil || n <= 0 {
				renderAPIError(w, api.NewError(400, "validation",
					"project_id must be a positive integer", "", nil))
				return
			}
			// Mirror the polling endpoint contract: an unknown positive
			// project_id is project_not_found, not an idle 200 stream.
			// Archived projects are also treated as not-found.
			if _, err := activeProjectByID(r.Context(), cfg.DB, n); err != nil {
				renderAPIError(w, err)
				return
			}
			projectID = n
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			renderAPIError(w, api.NewError(500, "internal", "streaming not supported", "", nil))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
			return
		}
		flusher.Flush()

		sub := cfg.Broadcaster.Subscribe(SubFilter{ProjectID: projectID})
		defer sub.Unsub()

		ctx := r.Context()
		hwm, err := cfg.DB.MaxEventID(ctx)
		if err != nil {
			return
		}

		resetTo, err := cfg.DB.PurgeResetCheck(ctx, cursor, projectID)
		if err != nil {
			return
		}
		if resetTo > 0 {
			writeResetFrame(w, resetTo)
			flusher.Flush()
			return
		}

		rows, err := cfg.DB.EventsAfter(ctx, db.EventsAfterParams{
			AfterID: cursor, ProjectID: projectID, ThroughID: hwm, Limit: sseDrainCap + 1,
		})
		if err != nil {
			return
		}

		if len(rows) == sseDrainCap+1 {
			writeResetFrame(w, hwm)
			flusher.Flush()
			return
		}

		lastSent := cursor
		for _, ev := range rows {
			writeEventFrame(w, ev)
			flusher.Flush()
			lastSent = ev.ID
		}

		runLivePhase(ctx, livePhaseDeps{w: w, flusher: flusher, cfg: cfg, ch: sub.Ch}, projectID, lastSent)
	}
}

// livePhaseDeps bundles the long-lived SSE writer state so runLivePhase stays
// within the project's positional-parameter limit.
type livePhaseDeps struct {
	w       http.ResponseWriter
	flusher http.Flusher
	cfg     ServerConfig
	ch      <-chan StreamMsg
}

// runLivePhase delivers events from deps.ch in canonical DB order. Each event
// wakeup triggers EventsAfter(lastSent, projectID, ThroughID: msg.Event.ID),
// which catches reordered broadcasts and coalesces bursts. Resets are
// terminal: emit the frame and return.
//
// lastSent enters as the id of the last drained frame (or cursor when the
// drain was empty). It tracks server-side state for de-duplication; the
// client's Last-Event-ID only advances on frames the client actually
// receives.
func runLivePhase(ctx context.Context, deps livePhaseDeps, projectID, lastSent int64) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(deps.w, ": keepalive\n\n"); err != nil {
				return
			}
			deps.flusher.Flush()
		case msg, ok := <-deps.ch:
			if !ok {
				return // overflow disconnect
			}
			switch msg.Kind {
			case "reset":
				writeResetFrame(deps.w, msg.ResetID)
				deps.flusher.Flush()
				return
			case "event":
				if msg.Event == nil {
					continue
				}
				// Defensive ordering: a concurrent purge can commit before
				// this event's broadcast is processed (broadcaster lock race
				// between two mutation goroutines). PurgeResetCheck makes the
				// reset terminal here so the client cannot receive a
				// post-purge frame, disconnect, and reconnect with
				// Last-Event-ID past the reset cursor — which would
				// permanently silence sync.reset_required.
				resetTo, err := deps.cfg.DB.PurgeResetCheck(ctx, lastSent, projectID)
				if err != nil {
					return
				}
				if resetTo > 0 {
					writeResetFrame(deps.w, resetTo)
					deps.flusher.Flush()
					return
				}
				// Loop until we've drained every row at or below the wakeup's
				// id. Without this, a single broadcast carrying >sseLiveBatch
				// pending events would leave the tail in the DB until the next
				// wakeup, leaving consumers indefinitely behind.
				through := msg.Event.ID
				for {
					rows, err := deps.cfg.DB.EventsAfter(ctx, db.EventsAfterParams{
						AfterID:   lastSent,
						ProjectID: projectID,
						ThroughID: through,
						Limit:     sseLiveBatch,
					})
					if err != nil {
						return
					}
					for _, ev := range rows {
						writeEventFrame(deps.w, ev)
						deps.flusher.Flush()
						lastSent = ev.ID
					}
					if len(rows) < sseLiveBatch {
						break
					}
				}
			}
		}
	}
}

func acceptableForSSE(accept string) bool {
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mt == "text/event-stream" || mt == "*/*" {
			return true
		}
	}
	return false
}

// parseSSECursor parses the SSE resume cursor from Last-Event-ID and/or
// ?after_id. Both are presence-based (per CursorConflict semantics): a
// present-but-empty value is a 400 validation error rather than a silent
// "no cursor". Caller is responsible for checking the cursor_conflict
// case (both present) before invoking this.
func parseSSECursor(r *http.Request) (int64, error) {
	var cursor int64
	if vs, ok := r.Header[http.CanonicalHeaderKey("Last-Event-ID")]; ok {
		v := ""
		if len(vs) > 0 {
			v = vs[0]
		}
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n < 0 {
			return 0, api.NewError(400, "validation",
				"Last-Event-ID must be a non-negative integer", "", nil)
		}
		cursor = n
	}
	if vs, ok := r.URL.Query()["after_id"]; ok {
		v := ""
		if len(vs) > 0 {
			v = vs[0]
		}
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n < 0 {
			return 0, api.NewError(400, "validation",
				"after_id must be a non-negative integer", "", nil)
		}
		cursor = n
	}
	return cursor, nil
}

func renderAPIError(w http.ResponseWriter, e error) {
	var ae *api.APIError
	if !errors.As(e, &ae) {
		api.WriteEnvelope(w, 500, "internal", e.Error())
		return
	}
	api.WriteEnvelope(w, ae.Status, ae.Code, ae.Message)
}

func writeEventFrame(w io.Writer, e db.Event) {
	body, _ := json.Marshal(eventToEnvelope(e))
	_, _ = w.Write(sseFrameBytes(e.ID, e.Type, body))
}

func writeResetFrame(w io.Writer, resetID int64) {
	body, _ := json.Marshal(api.EventReset{EventID: resetID, ResetAfterID: resetID})
	_, _ = w.Write(sseFrameBytes(resetID, "sync.reset_required", body))
}

// sseFrameBytes builds an SSE frame as raw bytes. Routed through []byte +
// w.Write rather than fmt.Fprintf to keep gosec's HTML-XSS taint analyzer
// (G705) from flagging the wire-format writers.
func sseFrameBytes(id int64, eventType string, data []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("id: ")
	buf.WriteString(strconv.FormatInt(id, 10))
	buf.WriteString("\nevent: ")
	buf.WriteString(eventType)
	buf.WriteString("\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.Bytes()
}
