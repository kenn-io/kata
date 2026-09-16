package jsonl

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/shortid"
	katauid "go.kenn.io/kata/internal/uid"
)

// applyCutoverV7toV8 transforms a decoded v7-or-earlier envelope stream into a
// v8-compatible one in place: it validates project names against the new
// `#`-free invariant, backfills issue UIDs for pre-v2 envelopes, derives each
// issue's short_id by replaying the §5 auto-extend algorithm in
// ULID-ascending order within each project, and rewrites v7-shape event
// payloads (numeric peer refs) into v8 shape (short_id + uid). The
// transformation runs before any SQL inserts so a bad project name fails the
// import before mutating the target.
//
// Envelope Data is rewritten in place (legacy `number` stripped, new
// `short_id` set on issues; legacy `*_number` peer refs replaced with
// `*_short_id` + `*_uid` on events) so the downstream importEnvelope loop
// sees current-version-shaped envelopes without any version-aware special
// cases.
func applyCutoverV7toV8(envs []Envelope) error {
	if err := validateNoHashInProjectNames(envs); err != nil {
		return err
	}
	lookup, err := deriveShortIDsForIssues(envs)
	if err != nil {
		return err
	}
	return rewriteV7EventPayloads(envs, lookup)
}

// issueLookupKey identifies an issue by (project_id, per-project number) —
// the v7 peer-reference shape. The cutover builds a per-project map so
// event-payload rewrites can find each peer's derived short_id + uid.
type issueLookupKey struct {
	projectID int64
	number    int64
}

// issueLookupInfo holds the v8 identifiers for a v7 issue: the derived
// short_id and the (possibly-backfilled) UID.
type issueLookupInfo struct {
	shortID string
	uid     string
}

// validateNoHashInProjectNames walks every project envelope and rejects names
// containing '#' before any inserts run. The error names the offending
// project so the operator can rename it before retrying.
func validateNoHashInProjectNames(envs []Envelope) error {
	for _, env := range envs {
		if env.Kind != KindProject {
			continue
		}
		var rec struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(env.Data, &rec); err != nil {
			return fmt.Errorf("decode project name for cutover validation: %w", err)
		}
		if strings.Contains(rec.Name, "#") {
			return fmt.Errorf("project name %q must not contain '#'; rename before cutover", rec.Name)
		}
	}
	return nil
}

// deriveShortIDsForIssues replays the §5 auto-extend algorithm per project in
// ULID-ascending order. Each issue envelope's Data is rewritten in place with
// the derived short_id field; the legacy `number` field is stripped. The
// returned map keys each issue by (project_id, number) so the event-payload
// rewrite step can resolve v7-shape peer refs.
//
// Issues are grouped by `project_id`, backfilled with a UID via FromStableSeed
// when missing (pre-v2 envelopes), then sorted by UID ascending. For each
// issue in order, the shortest length L (starting at shortid.MinLength) whose
// lowercased UID suffix is unique within the same project's already-assigned
// short_ids is the issue's short_id.
func deriveShortIDsForIssues(envs []Envelope) (map[issueLookupKey]issueLookupInfo, error) {
	// Snapshot every issue envelope's index, UID, project_id, and v7 number
	// so we can process them in cross-envelope ULID order without losing the
	// slot to write back to, and build the lookup keyed by the v7 number.
	type pendingIssue struct {
		envIdx    int                       // index into envs
		uid       string                    // backfilled if missing
		projectID int64                     // for grouping
		number    int64                     // v7 per-project number (0 if absent)
		raw       map[string]jsontext.Value // decoded data, mutated in place
	}
	pending := []pendingIssue{}
	for i, env := range envs {
		if env.Kind != KindIssue {
			continue
		}
		var raw map[string]jsontext.Value
		if err := json.Unmarshal(env.Data, &raw); err != nil {
			return nil, fmt.Errorf("decode issue for cutover: %w", err)
		}
		uid, err := issueUIDFromRaw(raw)
		if err != nil {
			return nil, fmt.Errorf("derive issue uid for cutover: %w", err)
		}
		var projectID int64
		if v, ok := raw["project_id"]; ok {
			if err := json.Unmarshal(v, &projectID); err != nil {
				return nil, fmt.Errorf("decode issue project_id for cutover: %w", err)
			}
		}
		var number int64
		if v, ok := raw["number"]; ok {
			if err := json.Unmarshal(v, &number); err != nil {
				return nil, fmt.Errorf("decode issue number for cutover: %w", err)
			}
		}
		pending = append(pending, pendingIssue{
			envIdx:    i,
			uid:       uid,
			projectID: projectID,
			number:    number,
			raw:       raw,
		})
	}
	// Group by project_id; sort each group by UID ascending so auto-extend
	// is deterministic. The canonical ordering matches a fresh
	// ULID-ascending replay (spec §4.2).
	byProject := map[int64][]int{}
	for i, p := range pending {
		byProject[p.projectID] = append(byProject[p.projectID], i)
	}
	lookup := make(map[issueLookupKey]issueLookupInfo, len(pending))
	for _, idxs := range byProject {
		sort.SliceStable(idxs, func(a, b int) bool {
			return pending[idxs[a]].uid < pending[idxs[b]].uid
		})
		assigned := map[string]bool{}
		for _, idx := range idxs {
			p := pending[idx]
			short, err := deriveAutoExtendShortID(p.uid, assigned)
			if err != nil {
				return nil, fmt.Errorf("derive short_id for uid %s: %w", p.uid, err)
			}
			assigned[short] = true
			// Rewrite envelope data in place: drop `number`, set
			// `short_id`, set `uid` (in case it was backfilled for v1).
			delete(p.raw, "number")
			p.raw["short_id"] = jsontext.Value(`"` + short + `"`)
			p.raw["uid"] = jsontext.Value(`"` + p.uid + `"`)
			data, err := json.Marshal(p.raw, json.Deterministic(true))
			if err != nil {
				return nil, fmt.Errorf("re-marshal cutover issue: %w", err)
			}
			envs[p.envIdx].Data = data
			if p.number > 0 {
				lookup[issueLookupKey{projectID: p.projectID, number: p.number}] = issueLookupInfo{
					shortID: short,
					uid:     p.uid,
				}
			}
		}
	}
	return lookup, nil
}

// rewriteV7EventPayloads walks every event envelope and rewrites its payload
// from v7 (numeric peer refs) to v8 (short_id + uid). Affected payloads:
//   - issue.linked / issue.unlinked: from_number / to_number → from_short_id /
//     from_uid / to_short_id / to_uid (plus the carry-through link_id/type).
//   - issue.created: links[].to_number → to_short_id / to_issue_uid.
//   - issue.links_changed: numeric parent_set / parent_removed and []int
//     blocks_added etc. → string short_ids paired with *_uid / *_uids.
//
// Missing lookups (e.g., event references an issue that's not in the export)
// leave the legacy field in place rather than failing the whole cutover; the
// stale field is harmless on read and the event remains in the events table.
func rewriteV7EventPayloads(envs []Envelope, lookup map[issueLookupKey]issueLookupInfo) error {
	for i, env := range envs {
		if env.Kind != KindEvent {
			continue
		}
		var raw map[string]jsontext.Value
		if err := json.Unmarshal(env.Data, &raw); err != nil {
			return fmt.Errorf("decode event %d for cutover: %w", i, err)
		}
		var projectID int64
		if v, ok := raw["project_id"]; ok {
			if err := json.Unmarshal(v, &projectID); err != nil {
				return fmt.Errorf("decode event project_id for cutover: %w", err)
			}
		}
		var evType string
		if v, ok := raw["type"]; ok {
			if err := json.Unmarshal(v, &evType); err != nil {
				return fmt.Errorf("decode event type for cutover: %w", err)
			}
		}
		payloadRaw, ok := raw["payload"]
		if !ok {
			continue
		}
		var payload map[string]jsontext.Value
		if err := json.Unmarshal(payloadRaw, &payload); err != nil {
			// Payload isn't an object (e.g., legacy empty payload or scalar) —
			// nothing to rewrite.
			continue
		}
		if payload == nil {
			// Payload was JSON null; nothing to rewrite.
			continue
		}
		changed := false
		switch evType {
		case "issue.linked", "issue.unlinked":
			changed = rewriteLinkEventPayload(payload, projectID, lookup) || changed
		case "issue.created":
			changed = rewriteCreatedEventPayload(payload, projectID, lookup) || changed
		case "issue.links_changed":
			changed = rewriteLinksChangedPayload(payload, projectID, lookup) || changed
		}
		if !changed {
			continue
		}
		newPayload, err := json.Marshal(payload, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("re-marshal event payload: %w", err)
		}
		raw["payload"] = newPayload
		data, err := json.Marshal(raw, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("re-marshal event envelope: %w", err)
		}
		envs[i].Data = data
	}
	return nil
}

// rewriteLinkEventPayload converts the v7 issue.linked / issue.unlinked
// payload (from_number / to_number) to v8 shape (from_short_id, from_uid,
// to_short_id, to_uid). Returns true if any rewrite happened.
func rewriteLinkEventPayload(payload map[string]jsontext.Value, projectID int64, lookup map[issueLookupKey]issueLookupInfo) bool {
	changed := false
	if info, ok := resolveNumberRef(payload, "from_number", projectID, lookup); ok {
		setStringField(payload, "from_short_id", info.shortID)
		setStringField(payload, "from_uid", info.uid)
		delete(payload, "from_number")
		changed = true
	}
	if info, ok := resolveNumberRef(payload, "to_number", projectID, lookup); ok {
		setStringField(payload, "to_short_id", info.shortID)
		setStringField(payload, "to_uid", info.uid)
		delete(payload, "to_number")
		changed = true
	}
	return changed
}

// rewriteCreatedEventPayload rewrites the links array of an issue.created
// payload: each {type, to_number} entry becomes {type, to_short_id,
// to_issue_uid}. Other top-level fields (labels, idempotency_key, etc.) are
// untouched.
func rewriteCreatedEventPayload(payload map[string]jsontext.Value, projectID int64, lookup map[issueLookupKey]issueLookupInfo) bool {
	linksRaw, ok := payload["links"]
	if !ok {
		return false
	}
	var links []map[string]jsontext.Value
	if err := json.Unmarshal(linksRaw, &links); err != nil {
		return false
	}
	changed := false
	for i := range links {
		if links[i] == nil {
			continue
		}
		info, found := resolveNumberRef(links[i], "to_number", projectID, lookup)
		if !found {
			continue
		}
		setStringField(links[i], "to_short_id", info.shortID)
		setStringField(links[i], "to_issue_uid", info.uid)
		delete(links[i], "to_number")
		changed = true
	}
	if !changed {
		return false
	}
	out, err := json.Marshal(links, json.Deterministic(true))
	if err != nil {
		return false
	}
	payload["links"] = out
	return true
}

// rewriteLinksChangedPayload converts v7 numeric parent/blocks/related fields
// to v8 string short_ids paired with parallel *_uid / *_uids arrays.
func rewriteLinksChangedPayload(payload map[string]jsontext.Value, projectID int64, lookup map[issueLookupKey]issueLookupInfo) bool {
	changed := false
	scalar := []struct{ from, uidKey string }{
		{"parent_set", "parent_set_uid"},
		{"parent_removed", "parent_removed_uid"},
	}
	for _, s := range scalar {
		if info, ok := resolveNumberRef(payload, s.from, projectID, lookup); ok {
			setStringField(payload, s.from, info.shortID)
			setStringField(payload, s.uidKey, info.uid)
			changed = true
		}
	}
	arrays := []struct{ from, uidsKey string }{
		{"blocks_added", "blocks_added_uids"},
		{"blocks_removed", "blocks_removed_uids"},
		{"blocked_by_added", "blocked_by_added_uids"},
		{"blocked_by_removed", "blocked_by_removed_uids"},
		{"related_added", "related_added_uids"},
		{"related_removed", "related_removed_uids"},
	}
	for _, a := range arrays {
		if rewriteNumberArray(payload, a.from, a.uidsKey, projectID, lookup) {
			changed = true
		}
	}
	return changed
}

// resolveNumberRef reads a numeric peer-ref field from payload and looks up
// its (short_id, uid). Returns (info, true) on success. Returns (zero, false)
// when the field is absent, isn't a number, or the lookup misses.
func resolveNumberRef(payload map[string]jsontext.Value, field string, projectID int64, lookup map[issueLookupKey]issueLookupInfo) (issueLookupInfo, bool) {
	v, ok := payload[field]
	if !ok {
		return issueLookupInfo{}, false
	}
	var n int64
	if err := json.Unmarshal(v, &n); err != nil {
		return issueLookupInfo{}, false
	}
	if n <= 0 {
		return issueLookupInfo{}, false
	}
	info, ok := lookup[issueLookupKey{projectID: projectID, number: n}]
	return info, ok
}

// rewriteNumberArray converts a []int64 field of v7 peer numbers into a
// []string of short_ids in place, and writes the parallel []string of UIDs
// into uidsKey. Returns true if a rewrite occurred.
func rewriteNumberArray(payload map[string]jsontext.Value, field, uidsKey string, projectID int64, lookup map[issueLookupKey]issueLookupInfo) bool {
	raw, ok := payload[field]
	if !ok {
		return false
	}
	var nums []int64
	if err := json.Unmarshal(raw, &nums); err != nil {
		// Already a []string — nothing to do.
		return false
	}
	if len(nums) == 0 {
		return false
	}
	shortIDs := make([]string, 0, len(nums))
	uids := make([]string, 0, len(nums))
	for _, n := range nums {
		if n <= 0 {
			continue
		}
		info, found := lookup[issueLookupKey{projectID: projectID, number: n}]
		if !found {
			continue
		}
		shortIDs = append(shortIDs, info.shortID)
		uids = append(uids, info.uid)
	}
	if len(shortIDs) == 0 {
		return false
	}
	shortsJSON, err := json.Marshal(shortIDs)
	if err != nil {
		return false
	}
	uidsJSON, err := json.Marshal(uids)
	if err != nil {
		return false
	}
	payload[field] = shortsJSON
	payload[uidsKey] = uidsJSON
	return true
}

// setStringField writes a string value into a jsontext.Value map. The
// quoted/escaped path goes through json.Marshal so unusual characters are
// handled correctly.
func setStringField(m map[string]jsontext.Value, key, value string) {
	bs, err := json.Marshal(value)
	if err != nil {
		return
	}
	m[key] = bs
}

// issueUIDFromRaw reads the issue envelope's uid field, backfilling via
// FromStableSeed when absent (pre-v2 envelopes carried `(project_id, number,
// created_at)` and no UID). The backfill rule mirrors fillIssueUID so the
// cutover and the per-envelope import path agree on UIDs.
func issueUIDFromRaw(raw map[string]jsontext.Value) (string, error) {
	if v, ok := raw["uid"]; ok {
		var u string
		if err := json.Unmarshal(v, &u); err != nil {
			return "", fmt.Errorf("decode issue uid: %w", err)
		}
		if u != "" {
			return u, nil
		}
	}
	// Pre-v2 path: derive from (project_id, number, created_at).
	var projectID, number int64
	var createdAt string
	if v, ok := raw["project_id"]; ok {
		if err := json.Unmarshal(v, &projectID); err != nil {
			return "", fmt.Errorf("decode issue project_id: %w", err)
		}
	}
	if v, ok := raw["number"]; ok {
		if err := json.Unmarshal(v, &number); err != nil {
			return "", fmt.Errorf("decode issue number: %w", err)
		}
	}
	if v, ok := raw["created_at"]; ok {
		if err := json.Unmarshal(v, &createdAt); err != nil {
			return "", fmt.Errorf("decode issue created_at: %w", err)
		}
	}
	t, err := parseExportTime(createdAt)
	if err != nil {
		return "", fmt.Errorf("parse issue created_at: %w", err)
	}
	return katauid.FromStableSeed([]byte(fmt.Sprintf("issue:%d:%d", projectID, number)), t)
}

// deriveAutoExtendShortID returns the shortest length-L suffix of uid (lowercased)
// from shortid.MinLength to shortid.MaxLength that is not already in
// assigned. The caller owns the assigned set and adds the returned value
// after each call.
func deriveAutoExtendShortID(uid string, assigned map[string]bool) (string, error) {
	for length := shortid.MinLength; length <= shortid.MaxLength; length++ {
		candidate, err := shortid.Derive(uid, length)
		if err != nil {
			return "", err
		}
		if !assigned[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("auto-extend exhausted")
}
