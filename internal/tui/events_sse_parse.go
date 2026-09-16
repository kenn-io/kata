package tui

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"strconv"
	"strings"
)

// frameKind discriminates an SSE frame's purpose. frameReset is the
// terminal sync.reset_required frame that drops the cache.
type frameKind int

const (
	frameEvent frameKind = iota
	frameReset
)

// frame is the parsed shape of one SSE event block. id mirrors the
// "id:" line so we can resume via Last-Event-ID. The reset frame's
// reset_after_id payload field is intentionally ignored: it is the
// daemon's contract (api.EventReset.EventID == ResetAfterID) that the
// frame's id: line is the authoritative resume cursor, and the consumer
// already updates Last-Event-ID off id: on every frame.
type frame struct {
	kind      frameKind
	id        int64
	eventType string
	data      []byte
}

// sseEventPayload mirrors the fields of api.EventEnvelope the TUI
// inspects. Lives here so the parser does not pull internal/api into
// the TUI tree.
type sseEventPayload struct {
	Type            string         `json:"type"`
	ProjectID       int64          `json:"project_id"`
	ProjectUID      string         `json:"project_uid,omitempty"`
	IssueShortID    *string        `json:"issue_short_id,omitempty"`
	IssueUID        string         `json:"issue_uid,omitempty"`
	RelatedIssueUID string         `json:"related_issue_uid,omitempty"`
	Payload         jsontext.Value `json:"payload,omitempty"`
}

// errSSEEOF is the sentinel readNextFrame returns when the underlying
// reader is exhausted with no in-progress frame.
var errSSEEOF = errors.New("sse: stream ended")

// parseAllFrames consumes r to EOF and returns every valid frame. A
// test-only entry point — production reads frames one at a time so the
// consumer can dispatch as they arrive.
func parseAllFrames(r io.Reader) ([]frame, error) {
	br := bufio.NewReader(r)
	var out []frame
	for {
		f, err := readNextFrame(br)
		if errors.Is(err, errSSEEOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, f)
	}
}

// readNextFrame reads one frame off br. Malformed frames (no data line
// or blank event type) reset on the blank-line terminator and continue
// so a single bad frame can't wedge the long-lived consumer.
func readNextFrame(br *bufio.Reader) (frame, error) {
	cur := frame{}
	var hasEvent, hasData bool
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return frame{}, errSSEEOF
			}
			return frame{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !hasEvent || !hasData {
				cur, hasEvent, hasData = frame{}, false, false
				continue
			}
			return finalizeFrame(cur), nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		applySSEField(&cur, line, &hasEvent, &hasData)
	}
}

// applySSEField mutates cur for a single id/event/data line. Unknown
// fields are ignored per the SSE spec.
func applySSEField(cur *frame, line string, hasEvent, hasData *bool) {
	switch {
	case strings.HasPrefix(line, "id: "):
		if n, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64); err == nil {
			cur.id = n
		}
	case strings.HasPrefix(line, "event: "):
		v := strings.TrimPrefix(line, "event: ")
		if v != "" {
			cur.eventType = v
			*hasEvent = true
		}
	case strings.HasPrefix(line, "data: "):
		cur.data = []byte(strings.TrimPrefix(line, "data: "))
		*hasData = true
	}
}

// finalizeFrame classifies the frame. sync.reset_required becomes
// frameReset; everything else is frameEvent.
func finalizeFrame(f frame) frame {
	if f.eventType == "sync.reset_required" {
		f.kind = frameReset
		return f
	}
	f.kind = frameEvent
	return f
}

// decodeEventReceived parses the JSON body into eventReceivedMsg.
// Missing fields fall through as zero values.
//
// issue.created events carry their initial parent link (when present)
// folded into a `links` array on the payload — see
// internal/db/queries.go::buildCreatedPayload. The TUI's open-detail
// refetch logic needs that parent reference to know whether the new
// child belongs to the issue currently on screen, so we surface the
// first parent entry as msg.link with FromIssueUID filled in from the
// new issue's own UID (the payload only carries to_issue_uid; the
// from is implicit). issue.linked / issue.unlinked retain their
// original single-link shape (link object directly on payload).
func decodeEventReceived(f frame) eventReceivedMsg {
	var p sseEventPayload
	_ = json.Unmarshal(f.data, &p)
	out := eventReceivedMsg{
		eventType:       p.Type,
		projectID:       p.ProjectID,
		projectUID:      p.ProjectUID,
		issueUID:        p.IssueUID,
		relatedIssueUID: p.RelatedIssueUID,
	}
	if p.IssueShortID != nil {
		out.issueShortID = *p.IssueShortID
	}
	if p.Type == "issue.linked" || p.Type == "issue.unlinked" {
		var link linkPayload
		if len(p.Payload) > 0 && json.Unmarshal(p.Payload, &link) == nil {
			out.link = &link
		}
	}
	if p.Type == "issue.created" && len(p.Payload) > 0 {
		out.link = parentLinkFromCreatedPayload(p.Payload, out.issueShortID, out.issueUID)
		// Beyond the first parent link (rendered into out.link for the
		// parent-specific refetch path), the issue.created payload may
		// reference non-parent peers via --blocks / --blocked-by /
		// --related. Surface every peer through the same linksChanged.Refs
		// slice issue.links_changed uses, so an open detail pane on a
		// peer issue refetches when someone else creates an issue that
		// links to it.
		if refs, refUIDs := createdPeerRefsFromPayload(p.Payload); len(refs) > 0 {
			out.linksChanged = &linksChangedParents{Refs: refs, RefUIDs: refUIDs}
		}
	}
	if p.Type == "issue.links_changed" && len(p.Payload) > 0 {
		out.linksChanged = parentEndpointsFromLinksChangedPayload(p.Payload)
	}
	return out
}

// createdPeerRefsFromPayload walks the `links` array of an issue.created
// payload and returns every referenced peer (short_id + UID). Unlike
// parentLinkFromCreatedPayload (which extracts only the first parent
// for the dedicated parent-pane refresh path), this picks up
// blocks / blocked_by / related peers so any open detail pane on
// the OTHER end of a create-time link refetches promptly.
//
// Returns parallel-indexed slices: peerShortIDs[i] and peerUIDs[i]
// describe the same link.
func createdPeerRefsFromPayload(payload []byte) (peerShortIDs []string, peerUIDs []string) {
	var body struct {
		Links []struct {
			Type       string `json:"type"`
			ToShortID  string `json:"to_short_id,omitempty"`
			ToIssueUID string `json:"to_issue_uid,omitempty"`
		} `json:"links"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, nil
	}
	if len(body.Links) == 0 {
		return nil, nil
	}
	peerShortIDs = make([]string, 0, len(body.Links))
	peerUIDs = make([]string, 0, len(body.Links))
	for _, l := range body.Links {
		if l.ToIssueUID != "" || l.ToShortID != "" {
			peerShortIDs = append(peerShortIDs, l.ToShortID)
			peerUIDs = append(peerUIDs, l.ToIssueUID)
		}
	}
	if len(peerShortIDs) == 0 {
		return nil, nil
	}
	return peerShortIDs, peerUIDs
}

// parentEndpointsFromLinksChangedPayload extracts every peer issue
// referenced by an issue.links_changed payload. parent_set and
// parent_removed are surfaced as dedicated fields (the parent slot
// needs both endpoints to refresh on a replace); blocks / blocked_by /
// related adds and removes are flattened into Refs/RefUIDs so the
// detail-refetch logic also invalidates panes on the OTHER end of
// those mutations.
//
// RefUIDs runs parallel to Refs (same length, same order). UIDs are
// canonical and the consumer uses them as the authoritative key.
//
// Returns nil when the payload has no peer references.
func parentEndpointsFromLinksChangedPayload(payload []byte) *linksChangedParents {
	var body struct {
		ParentSet            *string  `json:"parent_set,omitempty"`
		ParentSetUID         *string  `json:"parent_set_uid,omitempty"`
		ParentRemoved        *string  `json:"parent_removed,omitempty"`
		ParentRemovedUID     *string  `json:"parent_removed_uid,omitempty"`
		BlocksAdded          []string `json:"blocks_added,omitempty"`
		BlocksAddedUIDs      []string `json:"blocks_added_uids,omitempty"`
		BlocksRemoved        []string `json:"blocks_removed,omitempty"`
		BlocksRemovedUIDs    []string `json:"blocks_removed_uids,omitempty"`
		BlockedByAdded       []string `json:"blocked_by_added,omitempty"`
		BlockedByAddedUIDs   []string `json:"blocked_by_added_uids,omitempty"`
		BlockedByRemoved     []string `json:"blocked_by_removed,omitempty"`
		BlockedByRemovedUIDs []string `json:"blocked_by_removed_uids,omitempty"`
		RelatedAdded         []string `json:"related_added,omitempty"`
		RelatedAddedUIDs     []string `json:"related_added_uids,omitempty"`
		RelatedRemoved       []string `json:"related_removed,omitempty"`
		RelatedRemovedUIDs   []string `json:"related_removed_uids,omitempty"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil
	}
	out := &linksChangedParents{}
	if body.ParentSet != nil {
		out.Set = *body.ParentSet
		if body.ParentSetUID != nil {
			out.SetUID = *body.ParentSetUID
		}
		out.Refs = append(out.Refs, *body.ParentSet)
		out.RefUIDs = append(out.RefUIDs, deref(body.ParentSetUID))
	}
	if body.ParentRemoved != nil {
		out.Removed = *body.ParentRemoved
		if body.ParentRemovedUID != nil {
			out.RemovedUID = *body.ParentRemovedUID
		}
		out.Refs = append(out.Refs, *body.ParentRemoved)
		out.RefUIDs = append(out.RefUIDs, deref(body.ParentRemovedUID))
	}
	appendPeers(&out.Refs, &out.RefUIDs, body.BlocksAdded, body.BlocksAddedUIDs)
	appendPeers(&out.Refs, &out.RefUIDs, body.BlocksRemoved, body.BlocksRemovedUIDs)
	appendPeers(&out.Refs, &out.RefUIDs, body.BlockedByAdded, body.BlockedByAddedUIDs)
	appendPeers(&out.Refs, &out.RefUIDs, body.BlockedByRemoved, body.BlockedByRemovedUIDs)
	appendPeers(&out.Refs, &out.RefUIDs, body.RelatedAdded, body.RelatedAddedUIDs)
	appendPeers(&out.Refs, &out.RefUIDs, body.RelatedRemoved, body.RelatedRemovedUIDs)
	if len(out.Refs) == 0 {
		return nil
	}
	return out
}

// appendPeers extends refs/uids in lockstep. When uids is shorter than
// shorts, the missing slots are padded with "" so RefUIDs stays
// parallel to Refs. The daemon always emits matched-length slices in
// the post-kata#1 wire shape; the padding is defensive.
func appendPeers(refs *[]string, refUIDs *[]string, shorts, uids []string) {
	for i, s := range shorts {
		*refs = append(*refs, s)
		var u string
		if i < len(uids) {
			u = uids[i]
		}
		*refUIDs = append(*refUIDs, u)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// parentLinkFromCreatedPayload extracts the first parent link from an
// issue.created payload's `links` array. Returns nil when the payload
// has no parent link or fails to parse. fromShortID and fromIssueUID
// are the new issue's own identifiers — the payload only stores the
// to side, so we fill in from at decode time so downstream
// matchesIssue checks both ends.
func parentLinkFromCreatedPayload(payload []byte, fromShortID, fromIssueUID string) *linkPayload {
	var body struct {
		Links []struct {
			Type       string `json:"type"`
			ToShortID  string `json:"to_short_id,omitempty"`
			ToIssueUID string `json:"to_issue_uid,omitempty"`
		} `json:"links"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil
	}
	for _, l := range body.Links {
		if l.Type == "parent" {
			return &linkPayload{
				Type:         "parent",
				FromShortID:  fromShortID,
				ToShortID:    l.ToShortID,
				FromIssueUID: fromIssueUID,
				ToIssueUID:   l.ToIssueUID,
			}
		}
	}
	return nil
}
