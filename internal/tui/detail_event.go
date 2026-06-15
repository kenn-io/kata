package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

// eventDescriber returns the human-readable description for an event of
// a given type. The describer can read payload fields (label name,
// to_short_id, owner). Functions instead of plain strings keep
// "labeled %s" / "linked %s #N" / closed-with-reason all expressible
// without a branchy switch in eventDescription.
type eventDescriber func(e EventLogEntry) string

// staticDesc returns an eventDescriber that always emits s. Used for
// the simple cases (created, reopened, etc.) that don't need payload.
func staticDesc(s string) eventDescriber {
	return func(EventLogEntry) string { return s }
}

// payloadDesc returns an eventDescriber that emits "<prefix> <field>"
// where field is read out of the payload as a string.
func payloadDesc(prefix, field string) eventDescriber {
	return func(e EventLogEntry) string {
		return prefix + " " + payloadString(e, field)
	}
}

// eventDescribers is the per-type dispatch table for eventDescription.
// Unknown types fall through to a stripped "issue." prefix in
// eventDescription so the column always carries something readable.
var eventDescribers = map[string]eventDescriber{
	"issue.created":          staticDesc("created"),
	"issue.closed":           func(e EventLogEntry) string { return "closed" + reasonSuffix(e) },
	"issue.reopened":         staticDesc("reopened"),
	"issue.commented":        staticDesc("added comment"),
	"issue.labeled":          payloadDesc("labeled", "label"),
	"issue.unlabeled":        payloadDesc("unlabeled", "label"),
	"issue.linked":           func(e EventLogEntry) string { return "linked " + linkPayloadDesc(e) },
	"issue.unlinked":         func(e EventLogEntry) string { return "unlinked " + linkPayloadDesc(e) },
	"issue.links_changed":    linksChangedDesc,
	"issue.assigned":         payloadDesc("assigned", "owner"),
	"issue.unassigned":       staticDesc("unassigned"),
	"issue.priority_set":     prioritySetDesc,
	"issue.priority_cleared": priorityClearedDesc,
	"issue.updated":          staticDesc("updated"),
	"issue.soft_deleted":     staticDesc("deleted"),
	"issue.restored":         staticDesc("restored"),
}

// linksChangedDesc renders the aggregated issue.links_changed event from
// the PATCH path (`kata edit`) into a one-line summary that surfaces every
// add/remove direction. Each segment reads as "<verb> <type>:#<short_id>",
// so a single edit that swaps a parent and adds a related might render
// as "links: parent #abc4→#def4, +related #f00d".
func linksChangedDesc(e EventLogEntry) string {
	if e.Payload == nil {
		return "links changed"
	}
	parts := make([]string, 0, 8)
	if from, to, ok := payloadParentReplace(e); ok {
		parts = append(parts, fmt.Sprintf("parent #%s→#%s", from, to))
	} else if to, ok := payloadStringField(e, "parent_set"); ok {
		parts = append(parts, fmt.Sprintf("+parent #%s", to))
	} else if from, ok := payloadStringField(e, "parent_removed"); ok {
		parts = append(parts, fmt.Sprintf("-parent #%s", from))
	}
	parts = append(parts, linksChangedDirParts(e, "blocks_added", "+blocks")...)
	parts = append(parts, linksChangedDirParts(e, "blocks_removed", "-blocks")...)
	parts = append(parts, linksChangedDirParts(e, "blocked_by_added", "+blocked_by")...)
	parts = append(parts, linksChangedDirParts(e, "blocked_by_removed", "-blocked_by")...)
	parts = append(parts, linksChangedDirParts(e, "related_added", "+related")...)
	parts = append(parts, linksChangedDirParts(e, "related_removed", "-related")...)
	if len(parts) == 0 {
		return "links unchanged"
	}
	return "links: " + strings.Join(parts, ", ")
}

// payloadParentReplace returns (from, to) when both parent_removed and
// parent_set are present in one event — the parent-replace case. Returns
// ok=false when only one (or neither) is present, so callers can render
// the +parent / -parent variants instead.
func payloadParentReplace(e EventLogEntry) (from, to string, ok bool) {
	t, hasTo := payloadStringField(e, "parent_set")
	f, hasFrom := payloadStringField(e, "parent_removed")
	if hasTo && hasFrom {
		return f, t, true
	}
	return "", "", false
}

// linksChangedDirParts extracts a string-slice payload field
// (blocks_added, related_removed, etc.) and renders one segment per
// entry using the given verb-prefixed label (e.g. "+blocks #abc4").
func linksChangedDirParts(e EventLogEntry, key, label string) []string {
	refs := payloadStringSlice(e, key)
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("%s #%s", label, r))
	}
	return out
}

// payloadStringSlice reads a string-array field out of the event
// payload. Missing keys, non-array values, and a nil payload all
// return nil.
func payloadStringSlice(e EventLogEntry, key string) []string {
	if e.Payload == nil {
		return nil
	}
	raw, ok := e.Payload[key]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// prioritySetDesc renders "priority set to N" or "priority N → M" when the
// payload carries the prior value, so digest-style scrubbing surfaces both
// old and new priorities in one line.
func prioritySetDesc(e EventLogEntry) string {
	newP, ok := payloadInt(e, "priority")
	if !ok {
		return "priority changed"
	}
	if old, ok := payloadInt(e, "old_priority"); ok {
		return fmt.Sprintf("priority %d → %d", old, newP)
	}
	return fmt.Sprintf("priority set to %d", newP)
}

// priorityClearedDesc renders "priority cleared (was N)" when the payload
// carries the prior value, otherwise "priority cleared" alone.
func priorityClearedDesc(e EventLogEntry) string {
	if old, ok := payloadInt(e, "old_priority"); ok {
		return fmt.Sprintf("priority cleared (was %d)", old)
	}
	return "priority cleared"
}

// payloadInt reads a numeric field out of the event payload. Missing keys,
// non-numeric values, and a nil payload all return ok=false.
func payloadInt(e EventLogEntry, key string) (int64, bool) {
	if e.Payload == nil {
		return 0, false
	}
	v, ok := e.Payload[key]
	if !ok {
		return 0, false
	}
	return numberFromAny(v)
}

// payloadStringField reads a non-empty string field out of the event
// payload. Missing keys, non-string values, empty strings, and a nil
// payload all return ok=false. Distinct from payloadString so the
// links_changed describer can differentiate "absent" from
// "present-but-empty".
func payloadStringField(e EventLogEntry, key string) (string, bool) {
	if e.Payload == nil {
		return "", false
	}
	v, ok := e.Payload[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// eventDescription returns the type-specific short description used in
// the events tab. Unknown types fall back to a stripped "issue." prefix
// so the column always carries something readable.
func eventDescription(e EventLogEntry) string {
	if d, ok := eventDescribers[e.Type]; ok {
		return d(e)
	}
	return strings.TrimPrefix(e.Type, "issue.")
}

// eventChunkLines renders one event into the lines of its events-tab
// chunk. Most events are single-line: "[type] time actor — description".
// issue.closed events additionally emit indented continuation lines
// surfacing the close message and each evidence item so reviewers can
// audit closures directly from the events tab.
func eventChunkLines(e EventLogEntry, width int, isCursor bool) []string {
	// Type is daemon-authored; Actor and description interpolate
	// agent-authored payload fields — sanitize both.
	head := fmt.Sprintf("[%s] %s %s — %s",
		e.Type, fmtTime(e.CreatedAt),
		sanitizeForDisplay(e.Actor),
		sanitizeForDisplay(eventDescription(e)))
	lines := []string{applyActivityCursor(head, isCursor)}
	if e.Type == "issue.closed" {
		lines = append(lines, closeDetailLines(e, width)...)
	}
	return lines
}

// closeDetailLines returns the indented "message:" and "evidence:" rows
// that hang under an issue.closed header. Empty fields are skipped so a
// minimal close (e.g. TUI bypass with no message and no evidence) still
// renders cleanly as a single header line.
//
// Values are sanitized with sanitizeForLine (not sanitizeForDisplay) so
// embedded newlines render as the literal escape "\n" instead of
// spilling into extra physical rows that bypass chunk line-counting and
// cursor anchoring. Long values are then wrapped to width with a hanging
// indent so the events tab surfaces the full message on a narrow pane
// instead of clipping it with an ellipsis.
func closeDetailLines(e EventLogEntry, width int) []string {
	var out []string
	if msg := payloadString(e, "message"); msg != "" {
		out = append(out, wrapDetailRow("  message: ", sanitizeForLine(msg), width)...)
	}
	for _, line := range closeEvidenceSummaries(e) {
		out = append(out, wrapDetailRow("  evidence: ", sanitizeForLine(line), width)...)
	}
	return out
}

// wrapDetailRow renders one labeled detail row across as many physical
// rows as it takes to fit value into width. The first row carries
// prefix + the value head; continuation rows carry a hanging indent of
// the same display width so the value column stays vertically aligned.
// width <= 0 returns the row unwrapped so callers that don't yet know
// the pane width still emit readable output.
func wrapDetailRow(prefix, value string, width int) []string {
	if width <= 0 {
		return []string{prefix + value}
	}
	prefixW := runewidth.StringWidth(prefix)
	budget := width - prefixW
	if budget < 1 {
		budget = 1
	}
	parts := hardWrap(value, budget)
	if len(parts) == 0 {
		return []string{prefix}
	}
	out := make([]string, 0, len(parts))
	out = append(out, prefix+parts[0])
	hang := strings.Repeat(" ", prefixW)
	for _, p := range parts[1:] {
		out = append(out, hang+p)
	}
	return out
}

// closeEvidenceSummaries extracts the evidence array from an
// issue.closed event payload and returns one short label per item
// (e.g. "commit a1b2c3d", "reviewed-paths internal/db/queries.go").
// Missing or malformed evidence returns nil.
func closeEvidenceSummaries(e EventLogEntry) []string {
	if e.Payload == nil {
		return nil
	}
	raw, ok := e.Payload["evidence"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, formatEvidenceItem(m))
	}
	return out
}

// formatEvidenceItem renders one evidence map from the close payload
// (matching api.Evidence wire shape) into a short human label. The
// switch mirrors the EvidenceType union in internal/api/evidence.go;
// unknown types fall back to the raw type tag so future additions don't
// disappear silently.
func formatEvidenceItem(m map[string]any) string {
	t, _ := m["type"].(string)
	switch t {
	case "commit":
		return evidenceLabel(t, stringField(m, "sha"))
	case "pr":
		return evidenceLabel(t, stringField(m, "url"))
	case "test":
		return evidenceLabel(t, stringField(m, "command"))
	case "no-change-audit":
		return evidenceLabel(t, stringField(m, "rationale"))
	case "duplicate-of", "superseded-by":
		return evidenceLabel(t, stringField(m, "issue_ref"))
	case "reviewed-paths":
		return evidenceLabel(t, joinStringArray(m, "paths"))
	}
	if t == "" {
		return "?"
	}
	return t
}

// evidenceLabel joins a type tag with its payload value, dropping the
// trailing space when the value is empty so a malformed item renders as
// "commit" instead of "commit ".
func evidenceLabel(kind, value string) string {
	if value == "" {
		return kind
	}
	return kind + " " + value
}

// stringField reads a string field out of a generic map, returning ""
// for missing keys or non-string values.
func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// joinStringArray returns the comma-separated string-array field at key.
// Missing keys, non-array values, and arrays with no usable strings all
// return "".
func joinStringArray(m map[string]any, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	arr, ok := raw.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok && s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// reasonSuffix renders " (reason)" for closed events that carry one.
func reasonSuffix(e EventLogEntry) string {
	if r := payloadString(e, "reason"); r != "" {
		return " (" + r + ")"
	}
	return ""
}

// linkPayloadDesc formats "type #to_short_id" from a link.added /
// link.removed payload. Missing fields degrade gracefully — type
// alone, or just "?".
func linkPayloadDesc(e EventLogEntry) string {
	t := payloadString(e, "type")
	to, ok := readEventTargetShortID(e)
	if !ok {
		if t == "" {
			return "?"
		}
		return t
	}
	if t == "" {
		return "#" + to
	}
	return fmt.Sprintf("%s #%s", t, to)
}

// payloadString reads a string field out of the event payload. Missing
// keys, non-string values, and a nil payload all return "".
func payloadString(e EventLogEntry, key string) string {
	if e.Payload == nil {
		return ""
	}
	if v, ok := e.Payload[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// fmtTime is the compact timestamp used in tab content. The zero value
// renders as a dash so empty fixtures don't show "0001-01-01 00:00".
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04")
}

// eventJumpTarget reads the ref a jumpable event refers to, resolved under
// the current detail project. link.added / link.removed carry the peer
// (to_uid / to_short_id); we also accept issue_short_id for events whose
// subject IS the jump target.
func eventJumpTarget(events []EventLogEntry, idx int) (string, bool) {
	if idx < 0 || idx >= len(events) {
		return "", false
	}
	return readEventTargetRef(events[idx])
}

// readEventTargetRef pulls a jump ref out of e.Payload. A link peer's UID
// ("to_uid") is preferred over its short_id: an event row carries no peer
// project, so a bare short_id is resolved under the CURRENT detail project —
// which would open a same-suffix issue (or 404) when the peer is actually in
// another project. The peer UID is globally unique and the daemon's resolver
// scopes it to the URL project, so a same-project peer resolves while a
// foreign peer returns issue_not_found: the jump fails closed instead of
// landing on the wrong issue. Keys are checked most-specific first:
// to_uid, to_short_id (legacy/pre-UID link events, which predate
// cross-project links), then issue_short_id (subject events, same project).
func readEventTargetRef(e EventLogEntry) (string, bool) {
	if e.Payload == nil {
		return "", false
	}
	for _, k := range []string{"to_uid", "to_short_id", "issue_short_id"} {
		if v, ok := e.Payload[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// readEventTargetShortID pulls the peer's display short_id out of e.Payload
// for rendering link-event descriptions. Unlike readEventTargetRef (the jump
// path), this never returns a UID — the row text always shows the friendly
// short_id. Keys: to_short_id (link peer) then issue_short_id (subject).
func readEventTargetShortID(e EventLogEntry) (string, bool) {
	if e.Payload == nil {
		return "", false
	}
	for _, k := range []string{"to_short_id", "issue_short_id"} {
		if v, ok := e.Payload[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// numberFromAny widens a JSON-decoded number to int64.
func numberFromAny(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// linkJumpTarget returns the peer to navigate to from the link at idx.
// Outgoing links jump to the To peer; incoming links (where the To side is
// the current issue) jump to the From peer instead, so Enter on an
// "X blocks me" entry takes the user to X rather than re-opening the
// current issue. The chosen peer's project rides along so a cross-project
// link resolves under the peer's own project.
//
// "Is this the current issue?" is decided by UID when both ends carry one:
// short_ids are unique only within a project, so a foreign peer can share
// the current issue's bare short_id, and short_id equality alone would
// misroute an incoming cross-project link. Self-loop links resolve to the
// current issue and are rejected (no jump).
func linkJumpTarget(links []LinkEntry, idx int, currentShortID, currentUID string) (jumpTargetRef, bool) {
	if idx < 0 || idx >= len(links) {
		return jumpTargetRef{}, false
	}
	l := links[idx]
	peer := l.To
	if peerIsCurrent(l.To, currentShortID, currentUID) && l.From.ShortID != "" {
		peer = l.From
	}
	if peer.ShortID == "" || peerIsCurrent(peer, currentShortID, currentUID) {
		return jumpTargetRef{}, false
	}
	return jumpTargetRef{projectName: peer.Project, ref: peer.ShortID}, true
}

// peerIsCurrent reports whether a link peer is the issue currently open in
// the detail view. UID equality is authoritative when both the peer and
// the current issue carry a UID; otherwise it falls back to short_id
// equality (the only signal available for fixtures or pre-UID rows).
func peerIsCurrent(p LinkPeer, currentShortID, currentUID string) bool {
	if currentUID != "" && p.UID != "" {
		return p.UID == currentUID
	}
	return currentShortID != "" && p.ShortID == currentShortID
}
