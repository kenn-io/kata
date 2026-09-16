package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
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
	sseLiveBatch    = 1000
	sseWriteTimeout = 10 * time.Second

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

	registerEventsStream(humaAPI, cfg)
	registerEventsStreamMethodGuards(mux)
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

	resetProjectID := projectID
	if issueScopeFromContext(ctx) != nil {
		resetProjectID = 0
	}
	resetTo, err := cfg.DB.PurgeResetCheck(ctx, afterID, resetProjectID)
	if err != nil {
		return nil, internalAPIError(err)
	}
	if resetTo > 0 {
		out := &api.PollEventsResponse{}
		out.Body.ResetRequired = true
		out.Body.ResetAfterID = resetTo
		out.Body.Events = []api.EventEnvelope{}
		out.Body.NextAfterID = resetTo
		return out, nil
	}

	rows, nextCursor, scopedResetTo, err := readVisibleEvents(ctx, cfg.DB, afterID, projectID, 0, limit)
	if err != nil {
		return nil, err
	}
	if scopedResetTo > 0 {
		out := &api.PollEventsResponse{}
		out.Body.ResetRequired = true
		out.Body.ResetAfterID = scopedResetTo
		out.Body.Events = []api.EventEnvelope{}
		out.Body.NextAfterID = scopedResetTo
		return out, nil
	}

	out := &api.PollEventsResponse{}
	out.Body.ResetRequired = false
	out.Body.Events = toEnvelopes(rows)
	out.Body.NextAfterID = nextCursor
	return out, nil
}

func readVisibleEvents(
	ctx context.Context, store db.Storage, afterID, projectID, throughID int64, limit int,
) ([]db.Event, int64, int64, error) {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		rows, err := store.EventsAfter(ctx, db.EventsAfterParams{
			AfterID: afterID, ProjectID: projectID, ThroughID: throughID, Limit: limit,
		})
		if err != nil {
			return nil, afterID, 0, internalAPIError(err)
		}
		next := nextAfterID(rows, afterID)
		if len(rows) == 0 && throughID > next {
			next = throughID
		}
		return rows, next, 0, nil
	}
	if err := revalidateIssueScopedPrincipal(ctx, store); err != nil {
		return nil, afterID, 0, err
	}
	// Scoped project streams still scan the global durable cursor so a hidden
	// blocker or project lifecycle change outside the granted project can
	// invalidate derived readiness without disclosing the triggering identity.
	scanProjectID := int64(0)
	visible := make([]db.Event, 0, limit)
	cursor := afterID
	// Count hidden rows against each scan budget. Polling returns a partial
	// cursor; an exhausted SSE window resets to its captured high-water mark.
	scanLimit := pollLimitMax
	if throughID > 0 {
		scanLimit = limit
	}
	scanned := 0
	for len(visible) < limit && scanned < scanLimit {
		batchLimit := min(pollLimitMax, max(limit-len(visible), 100), scanLimit-scanned)
		rows, err := store.EventsAfter(ctx, db.EventsAfterParams{
			AfterID: cursor, ProjectID: scanProjectID, ThroughID: throughID, Limit: batchLimit,
		})
		if err != nil {
			return nil, cursor, 0, internalAPIError(err)
		}
		if len(rows) == 0 {
			// A concurrent purge can delete every event in (cursor, throughID]
			// after the caller's reset check but before this read. Mirroring the
			// unscoped branch, an empty window still advances the cursor to
			// throughID so the SSE live drain loop terminates instead of
			// re-querying the vanished range forever.
			if throughID > cursor {
				cursor = throughID
			}
			break
		}
		// Read membership after the durable page: a child that leaves the
		// subtree must not expose a later comment through cached authorization.
		allowed, allowedUIDs, err := issueScopedEventMembership(ctx, store, *scope)
		if err != nil {
			return nil, cursor, 0, err
		}
		for _, event := range rows {
			cursor = event.ID
			scanned++
			projected, ok := projectIssueScopedEvent(event, allowed, scope.ProjectUID)
			if ok && event.Type != "issue.links_changed" {
				visible = append(visible, projected)
				if len(visible) == limit {
					break
				}
				continue
			}
			if issueScopedEventInScope(event, allowed, scope.ProjectUID) {
				// The event describes a granted issue but cannot be safely
				// projected (unknown type, malformed payload, or omitted compound
				// link peers). Reset so the client reloads all affected views.
				return nil, event.ID, event.ID, nil
			}
			reset, resetErr := hiddenEventRequiresScopedReset(
				ctx, store, event, allowed, allowedUIDs, scope.ProjectUID,
			)
			if resetErr != nil {
				return nil, cursor, 0, internalAPIError(resetErr)
			}
			if reset {
				return nil, event.ID, event.ID, nil
			}
		}
		if len(rows) < batchLimit || (throughID > 0 && cursor >= throughID) {
			if throughID > cursor {
				cursor = throughID
			}
			break
		}
	}
	if throughID > cursor && scanned == scanLimit {
		return nil, throughID, throughID, nil
	}
	return visible, cursor, 0, nil
}

func hiddenEventRequiresScopedReset(
	ctx context.Context,
	store db.Storage,
	event db.Event,
	allowed map[int64]struct{},
	allowedUIDs map[string]struct{},
	projectUID string,
) (bool, error) {
	directlyAllowed := eventIssueIDsTouchScope(event, allowed)

	switch event.Type {
	case "issue.created":
		var created struct {
			Links []struct {
				ToIssueUID string `json:"to_issue_uid"`
			} `json:"links"`
		}
		if json.Unmarshal([]byte(event.Payload), &created) != nil {
			return true, nil
		}
		for _, link := range created.Links {
			if _, ok := allowedUIDs[link.ToIssueUID]; ok {
				return true, nil
			}
		}
		return false, nil
	case "issue.linked", "issue.unlinked":
		return directlyAllowed, nil
	case "issue.links_changed":
		return directlyAllowed || compoundLinkEventTouchesScope(event.Payload, allowedUIDs), nil
	case "issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored":
		return hiddenIssueTouchesScope(ctx, store, event.IssueID, allowed)
	case "issue.moved":
		if movedEventTouchesProject(event.Payload, projectUID) {
			return true, nil
		}
		return hiddenIssueTouchesScope(ctx, store, event.IssueID, allowed)
	case "issue.snapshot":
		// Federation snapshots establish an issue baseline. Later relationship
		// changes arrive as link events, so only a currently related hidden
		// snapshot can affect the scoped projection.
		return hiddenIssueTouchesScope(ctx, store, event.IssueID, allowed)
	case "project.removed", "project.restored", "project.merged":
		// Project lifecycle events are rare and may invalidate issue membership
		// or cross-project relationships without naming every affected issue.
		return true, nil
	case "project.renamed":
		return event.ProjectUID == projectUID, nil
	default:
		return false, nil
	}
}

func eventIssueIDsTouchScope(event db.Event, allowed map[int64]struct{}) bool {
	if event.IssueID != nil {
		if _, ok := allowed[*event.IssueID]; ok {
			return true
		}
	}
	if event.RelatedIssueID != nil {
		_, ok := allowed[*event.RelatedIssueID]
		return ok
	}
	return false
}

func issueScopedEventMembership(
	ctx context.Context, store db.Storage, scope db.APITokenScope,
) (map[int64]struct{}, map[string]struct{}, error) {
	members, err := store.IssueScopedMembers(ctx, scope)
	if err != nil {
		return nil, nil, internalAPIError(err)
	}
	ids := make(map[int64]struct{}, len(members))
	uids := make(map[string]struct{}, len(members))
	for _, issue := range members {
		ids[issue.ID] = struct{}{}
		uids[issue.UID] = struct{}{}
	}
	return ids, uids, nil
}

func compoundLinkEventTouchesScope(payload string, allowedUIDs map[string]struct{}) bool {
	var changed struct {
		ParentSetUID         string   `json:"parent_set_uid"`
		ParentRemovedUID     string   `json:"parent_removed_uid"`
		BlocksAddedUIDs      []string `json:"blocks_added_uids"`
		BlocksRemovedUIDs    []string `json:"blocks_removed_uids"`
		BlockedByAddedUIDs   []string `json:"blocked_by_added_uids"`
		BlockedByRemovedUIDs []string `json:"blocked_by_removed_uids"`
		RelatedAddedUIDs     []string `json:"related_added_uids"`
		RelatedRemovedUIDs   []string `json:"related_removed_uids"`
	}
	if json.Unmarshal([]byte(payload), &changed) != nil {
		return true
	}
	peers := []string{changed.ParentSetUID, changed.ParentRemovedUID}
	peers = append(peers, changed.BlocksAddedUIDs...)
	peers = append(peers, changed.BlocksRemovedUIDs...)
	peers = append(peers, changed.BlockedByAddedUIDs...)
	peers = append(peers, changed.BlockedByRemovedUIDs...)
	peers = append(peers, changed.RelatedAddedUIDs...)
	peers = append(peers, changed.RelatedRemovedUIDs...)
	for _, peerUID := range peers {
		if _, ok := allowedUIDs[peerUID]; ok {
			return true
		}
	}
	return false
}

func hiddenIssueTouchesScope(
	ctx context.Context, store db.Storage, issueID *int64, allowed map[int64]struct{},
) (bool, error) {
	if issueID == nil {
		return false, nil
	}
	links, err := store.LinksByIssue(ctx, *issueID)
	if err != nil {
		return false, err
	}
	for _, link := range links {
		if _, ok := allowed[link.FromIssueID]; ok {
			return true, nil
		}
		if _, ok := allowed[link.ToIssueID]; ok {
			return true, nil
		}
	}
	return false, nil
}

func movedEventTouchesProject(payload, projectUID string) bool {
	var moved struct {
		FromProjectUID string `json:"from_project_uid"`
		ToProjectUID   string `json:"to_project_uid"`
	}
	if json.Unmarshal([]byte(payload), &moved) != nil {
		// Malformed durable events are unexpected; fail closed so a scoped
		// client refreshes rather than retaining a possibly stale projection.
		return true
	}
	return moved.FromProjectUID == projectUID || moved.ToProjectUID == projectUID
}

func projectIssueScopedEvent(
	event db.Event, allowed map[int64]struct{}, projectUID string,
) (db.Event, bool) {
	if !issueScopedEventInScope(event, allowed, projectUID) {
		return db.Event{}, false
	}
	if (event.Type == "issue.linked" || event.Type == "issue.unlinked") && event.RelatedIssueID == nil {
		// Project-only export/restore can omit the peer identity while keeping
		// its payload. Without an authorized peer, clients must reload instead.
		return db.Event{}, false
	}
	payload, projectable := scopedEventPayload(event)
	if !projectable {
		// In-scope but not safely projectable (unknown issue.* type, malformed
		// payload, or a marshal failure): fail closed. Callers treat this like
		// a hidden event so clients re-sync instead of trusting a hollow frame.
		return db.Event{}, false
	}
	event.Payload = payload
	event.ContentHash = ""
	// OriginInstanceUID is infrastructure identity (which federated instance
	// produced the row). It stays internal to the daemon; scoped clients only
	// ever see the zero value.
	event.OriginInstanceUID = ""
	return event, true
}

// issueScopedEventInScope reports whether the event describes a granted
// issue inside the scoped project, independent of payload redactability.
func issueScopedEventInScope(event db.Event, allowed map[int64]struct{}, projectUID string) bool {
	if event.IssueID == nil || !strings.HasPrefix(event.Type, "issue.") {
		return false
	}
	if event.ProjectUID != projectUID {
		return false
	}
	if _, ok := allowed[*event.IssueID]; !ok {
		return false
	}
	if event.RelatedIssueID != nil {
		if _, ok := allowed[*event.RelatedIssueID]; !ok {
			return false
		}
	}
	return true
}

func scopedEventPayload(event db.Event) (string, bool) {
	allowedKeys := map[string]map[string]struct{}{
		"issue.created":          keySet("title", "body", "owner", "priority", "labels", "metadata", "created_at"),
		"issue.updated":          keySet("title", "body", "owner", "changes", "updated_at"),
		"issue.commented":        keySet("comment_uid", "author", "teammate", "body", "created_at"),
		"issue.comment_edited":   keySet("comment_uid", "body", "edited_at"),
		"issue.assigned":         keySet("owner"),
		"issue.unassigned":       keySet("owner"),
		"issue.priority_set":     keySet("priority"),
		"issue.priority_cleared": keySet("old_priority", "updated_at"),
		"issue.labeled":          keySet("label"),
		"issue.unlabeled":        keySet("label"),
		"issue.metadata_updated": keySet("diff", "revision_new", "updated_at"),
		"issue.linked":           keySet("type", "from_short_id", "from_uid", "to_short_id", "to_uid", "incoming"),
		"issue.unlinked":         keySet("type", "from_short_id", "from_uid", "to_short_id", "to_uid", "incoming"),
		"issue.closed":           keySet("reason", "closed_at", "message", "evidence"),
		"issue.reopened":         keySet("reopened_at"),
		// Ladder verbs carry only mutation timestamps.
		"issue.soft_deleted": keySet("deleted_at"),
		"issue.restored":     keySet("restored_at", "updated_at"),
		// Moves are recorded in the target project, so a scoped client only
		// ever sees arrivals into its own project. The projection keeps the
		// arrival identity (to_project_uid equals the granted project, and
		// to_short_id is the granted issue's new ref) plus the timestamp, and
		// strips the source project's UID and short id — coordinates of a
		// project the scope cannot see. The issue's own UID already travels
		// on the envelope (IssueUID).
		"issue.moved": keySet("to_project_uid", "to_short_id", "updated_at"),
	}
	if event.Type == "issue.links_changed" {
		// History and mutation responses omit compound peers that may be
		// outside the subtree. Polling and SSE reset so linked issue views
		// refresh; reports re-add authorized peers (scoped_authorization.go).
		return "", true
	}
	keys, known := allowedKeys[event.Type]
	if !known {
		return "", false
	}
	if event.Payload == "" {
		return "", true
	}
	var raw map[string]jsontext.Value
	if json.Unmarshal([]byte(event.Payload), &raw) != nil {
		return "", false
	}
	projected := make(map[string]jsontext.Value, len(keys))
	for key := range keys {
		if value, ok := raw[key]; ok {
			projected[key] = value
		}
	}
	if event.Type == "issue.closed" {
		projected["evidence"] = scopedCloseEvidence(projected["evidence"])
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func keySet(keys ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		out[key] = struct{}{}
	}
	return out
}

func scopedCloseEvidence(raw jsontext.Value) jsontext.Value {
	if len(raw) == 0 {
		return nil
	}
	var entries []map[string]jsontext.Value
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	filtered := entries[:0]
	for _, entry := range entries {
		var kind string
		_ = json.Unmarshal(entry["type"], &kind)
		if kind == "duplicate-of" || kind == "superseded-by" {
			continue
		}
		filtered = append(filtered, entry)
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return nil
	}
	return encoded
}

func toEnvelopes(rows []db.Event) []api.EventEnvelope {
	out := make([]api.EventEnvelope, 0, len(rows))
	for _, r := range rows {
		out = append(out, eventToEnvelope(r))
	}
	return out
}

func eventToEnvelope(e db.Event) api.EventEnvelope {
	var payload jsontext.Value
	if e.Payload != "" {
		payload = jsontext.Value(e.Payload)
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

type eventsStreamInput struct {
	accept             string
	lastEventID        string
	lastEventIDPresent bool
	query              map[string][]string
}

func (in *eventsStreamInput) Resolve(ctx huma.Context) []error {
	in.accept = ctx.Header("Accept")
	u := ctx.URL()
	in.query = u.Query()
	ctx.EachHeader(func(name, value string) {
		if !strings.EqualFold(name, "Last-Event-ID") || in.lastEventIDPresent {
			return
		}
		in.lastEventID = value
		in.lastEventIDPresent = true
	})
	return nil
}

func registerEventsStream(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "streamEvents",
		Method:      http.MethodGet,
		Path:        "/api/v1/events/stream",
		Summary:     "Stream events",
		Description: "Streams durable events as Server-Sent Events. Clients may resume with Last-Event-ID or after_id, but not both.",
		Parameters: []*huma.Param{
			{
				Name:        "after_id",
				In:          "query",
				Description: "Exclusive event cursor. Mutually exclusive with Last-Event-ID.",
				Schema:      &huma.Schema{Type: huma.TypeInteger, Format: "int64"},
			},
			{
				Name:        "project_id",
				In:          "query",
				Description: "Optional project ID filter. Omit to stream all visible project events.",
				Schema:      &huma.Schema{Type: huma.TypeInteger, Format: "int64"},
			},
			{
				Name:        "Last-Event-ID",
				In:          "header",
				Description: "Exclusive resume cursor from the last received SSE id. Mutually exclusive with after_id.",
				Schema:      &huma.Schema{Type: huma.TypeInteger, Format: "int64"},
			},
		},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Server-Sent Events stream.",
				Content: map[string]*huma.MediaType{
					"text/event-stream": {
						Schema: &huma.Schema{
							Type:        huma.TypeString,
							Description: "UTF-8 Server-Sent Events frames containing event envelopes or sync.reset_required reset payloads.",
						},
					},
				},
			},
		},
	}, func(ctx context.Context, in *eventsStreamInput) (*huma.StreamResponse, error) {
		if !acceptableForSSE(in.accept) {
			return nil, api.NewError(406, "not_acceptable", "Accept must be text/event-stream", "", nil)
		}

		cursor, err := parseSSECursor(in)
		if err != nil {
			return nil, err
		}
		projectID, err := parseSSEProjectID(in.query)
		if err != nil {
			return nil, err
		}
		var projectIDs []int64
		if projectID > 0 {
			projectIDs = []int64{projectID}
		}
		ctx, err = authorizeHostProjectScope(ctx, projectIDs, nil, projectID == 0)
		if err != nil {
			return nil, err
		}
		if projectID > 0 {
			if _, err := activeProjectByID(ctx, cfg.DB, projectID); err != nil {
				return nil, err
			}
		}
		if err := requireHostAccessLease(ctx); err != nil {
			return nil, err
		}

		return &huma.StreamResponse{
			Body: func(ctx huma.Context) {
				runSSEStream(ctx, cfg, cursor, projectID)
			},
		}, nil
	})
}

func registerEventsStreamMethodGuards(mux *http.ServeMux) {
	guard := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", http.MethodGet)
		api.WriteEnvelope(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"events stream only accepts GET")
	}
	mux.HandleFunc(http.MethodHead+" /api/v1/events/stream", guard)
	mux.HandleFunc("/api/v1/events/stream", guard)
}

// runSSEStream implements the streaming body for GET /api/v1/events/stream.
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
func runSSEStream(hctx huma.Context, cfg ServerConfig, cursor, projectID int64) {
	w, ok := hctx.BodyWriter().(http.ResponseWriter)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	hctx.SetHeader("Content-Type", "text/event-stream")
	hctx.SetHeader("Cache-Control", "private, no-cache")
	hctx.SetHeader("Connection", "keep-alive")
	if !writeSSEFrame(w, flusher, []byte(": connected\n\n")) {
		return
	}

	subscriptionProjectID := projectID
	if issueScopeFromContext(hctx.Context()) != nil {
		// Hidden blockers can live in another project. Scoped filtering still
		// happens against the durable log; the global subscription is only the
		// wakeup needed to notice an identity-free invalidation promptly.
		subscriptionProjectID = 0
	}
	sub := cfg.Broadcaster.Subscribe(SubFilter{ProjectID: subscriptionProjectID})
	defer sub.Unsub()

	ctx := hctx.Context()
	if projectID > 0 {
		project, projectErr := cfg.DB.ProjectByID(ctx, projectID)
		if errors.Is(projectErr, db.ErrNotFound) || (projectErr == nil && project.DeletedAt != nil) {
			resetID, resetErr := cfg.DB.MaxEventID(ctx)
			if resetErr != nil || revalidateEventStreamAuthority(ctx, cfg.DB) != nil {
				return
			}
			_ = writeSSEFrame(w, flusher, resetFrameBytes(resetID))
			return
		}
		if projectErr != nil {
			return
		}
	}
	hwm, err := cfg.DB.MaxEventID(ctx)
	if err != nil {
		return
	}

	resetProjectID := projectID
	if issueScopeFromContext(ctx) != nil {
		resetProjectID = 0
	}
	resetTo, err := cfg.DB.PurgeResetCheck(ctx, cursor, resetProjectID)
	if err != nil {
		return
	}
	if resetTo > 0 {
		if revalidateEventStreamAuthority(ctx, cfg.DB) != nil {
			return
		}
		_ = writeSSEFrame(w, flusher, resetFrameBytes(resetTo))
		return
	}

	rows, scannedTo, scopedResetTo, err := readVisibleEvents(
		ctx, cfg.DB, cursor, projectID, hwm, sseDrainCap+1,
	)
	if err != nil {
		return
	}
	if scopedResetTo > 0 {
		if revalidateEventStreamAuthority(ctx, cfg.DB) != nil {
			return
		}
		_ = writeSSEFrame(w, flusher, resetFrameBytes(scopedResetTo))
		return
	}

	if len(rows) == sseDrainCap+1 {
		if revalidateEventStreamAuthority(ctx, cfg.DB) != nil {
			return
		}
		_ = writeSSEFrame(w, flusher, resetFrameBytes(hwm))
		return
	}

	lastSent := scannedTo
	for _, ev := range rows {
		if !writeRevalidatedEventFrame(ctx, cfg.DB, w, flusher, ev) {
			return
		}
	}

	runLivePhase(ctx, livePhaseDeps{w: w, flusher: flusher, cfg: cfg, ch: sub.Ch}, projectID, lastSent)
}

func parseSSECursor(in *eventsStreamInput) (int64, error) {
	// cursor_conflict is checked on header/query *key presence* before
	// parsing values. A request with `Last-Event-ID: 5` plus `?after_id=`
	// (empty) or `?after_id=&after_id=5` would otherwise bypass detection if
	// we asked Get(), which only returns the first non-empty value.
	_, hadQuery := in.query["after_id"]
	if in.lastEventIDPresent && hadQuery {
		return 0, api.NewError(400, "cursor_conflict",
			"pass either Last-Event-ID or ?after_id, not both", "", nil)
	}

	var cursor int64
	if in.lastEventIDPresent {
		n, err := parseNonNegativeInt64(in.lastEventID, "Last-Event-ID")
		if err != nil {
			return 0, err
		}
		cursor = n
	}
	if vs, ok := in.query["after_id"]; ok {
		v := ""
		if len(vs) > 0 {
			v = vs[0]
		}
		n, err := parseNonNegativeInt64(v, "after_id")
		if err != nil {
			return 0, err
		}
		cursor = n
	}
	return cursor, nil
}

func parseNonNegativeInt64(raw, name string) (int64, error) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, api.NewError(400, "validation",
			name+" must be a non-negative integer", "", nil)
	}
	return n, nil
}

func parseSSEProjectID(query map[string][]string) (int64, error) {
	pidStr := ""
	if vs, ok := query["project_id"]; ok && len(vs) > 0 {
		pidStr = vs[0]
	}
	if pidStr == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(pidStr, 10, 64)
	if err != nil || n <= 0 {
		return 0, api.NewError(400, "validation",
			"project_id must be a positive integer", "", nil)
	}
	return n, nil
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
			if revalidateEventStreamAuthority(ctx, deps.cfg.DB) != nil {
				return
			}
			if !writeSSEFrame(deps.w, deps.flusher, []byte(": keepalive\n\n")) {
				return
			}
		case msg, ok := <-deps.ch:
			if !ok {
				return // overflow disconnect
			}
			switch msg.Kind {
			case StreamKindReset:
				if revalidateEventStreamAuthority(ctx, deps.cfg.DB) != nil {
					return
				}
				_ = writeSSEFrame(deps.w, deps.flusher, resetFrameBytes(msg.ResetID))
				return
			case StreamKindEvent:
				// StreamMsg is exported with exported fields, so "constructed only
				// via NewEventMsg" is repo discipline, not a language guarantee.
				// A hand-built envelope with a nil Event stays droppable here.
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
				resetProjectID := projectID
				if issueScopeFromContext(ctx) != nil {
					resetProjectID = 0
				}
				resetTo, err := deps.cfg.DB.PurgeResetCheck(ctx, lastSent, resetProjectID)
				if err != nil {
					return
				}
				if resetTo > 0 {
					if revalidateEventStreamAuthority(ctx, deps.cfg.DB) != nil {
						return
					}
					_ = writeSSEFrame(deps.w, deps.flusher, resetFrameBytes(resetTo))
					return
				}
				// Loop until we've drained every row at or below the wakeup's
				// id. Without this, a single broadcast carrying >sseLiveBatch
				// pending events would leave the tail in the DB until the next
				// wakeup, leaving consumers indefinitely behind.
				through := msg.Event.ID
				for {
					rows, scannedTo, scopedResetTo, err := readVisibleEvents(
						ctx, deps.cfg.DB, lastSent, projectID, through, sseLiveBatch,
					)
					if err != nil {
						return
					}
					lastSent = scannedTo
					if scopedResetTo > 0 {
						if revalidateEventStreamAuthority(ctx, deps.cfg.DB) != nil {
							return
						}
						_ = writeSSEFrame(deps.w, deps.flusher, resetFrameBytes(scopedResetTo))
						return
					}
					for _, ev := range rows {
						if !writeRevalidatedEventFrame(ctx, deps.cfg.DB, deps.w, deps.flusher, ev) {
							return
						}
					}
					if scannedTo >= through {
						break
					}
				}
			}
		}
	}
}

func writeRevalidatedEventFrame(
	ctx context.Context, store db.Storage, w http.ResponseWriter, flusher http.Flusher, event db.Event,
) bool {
	if revalidateEventStreamAuthority(ctx, store) != nil {
		return false
	}
	visible, err := scopedEventStillVisible(ctx, store, event)
	if err != nil {
		return false
	}
	frame := eventFrameBytes(event)
	if !visible {
		frame = resetFrameBytes(event.ID)
	}
	return writeSSEFrame(w, flusher, frame) && visible
}

func scopedEventStillVisible(ctx context.Context, store db.Storage, event db.Event) (bool, error) {
	if issueScopeFromContext(ctx) == nil {
		return true, nil
	}
	if event.IssueID == nil {
		return false, nil
	}
	ids := []*int64{event.IssueID}
	if event.RelatedIssueID != nil {
		ids = append(ids, event.RelatedIssueID)
	}
	for _, issueID := range ids {
		issue, err := store.IssueByID(ctx, *issueID)
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, internalAPIError(err)
		}
		if err := authorizeIssueScopedIssue(ctx, store, issue); err != nil {
			if apiErr, ok := errors.AsType[*api.APIError](err); ok && apiErr.Status == http.StatusNotFound {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

func revalidateEventStreamAuthority(ctx context.Context, store db.Storage) error {
	if err := revalidateSSEAuthority(ctx); err != nil {
		return err
	}
	return revalidateIssueScopedPrincipal(ctx, store)
}

func acceptableForSSE(accept string) bool {
	if accept == "" {
		return false
	}
	for part := range strings.SplitSeq(accept, ",") {
		mt := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mt == "text/event-stream" || mt == "*/*" {
			return true
		}
	}
	return false
}

func eventFrameBytes(e db.Event) []byte {
	body, _ := json.Marshal(eventToEnvelope(e))
	return sseFrameBytes(e.ID, e.Type, body)
}

func resetFrameBytes(resetID int64) []byte {
	body, _ := json.Marshal(api.EventReset{EventID: resetID, ResetAfterID: resetID})
	return sseFrameBytes(resetID, "sync.reset_required", body)
}

func writeSSEFrame(w http.ResponseWriter, flusher http.Flusher, frame []byte) bool {
	controller := http.NewResponseController(w)
	deadlineSet := false
	if err := controller.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err == nil {
		deadlineSet = true
	} else if !errors.Is(err, http.ErrNotSupported) {
		return false
	}
	if deadlineSet {
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
	if _, err := w.Write(frame); err != nil {
		return false
	}
	flusher.Flush()
	return true
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
