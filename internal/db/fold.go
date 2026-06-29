package db

import (
	"encoding/json"
	"fmt"
	"sort"
)

// FoldEvents applies events in deterministic HLC order and returns the resulting
// replay projection.
func FoldEvents(events []FoldEvent) FoldProjection {
	p := FoldProjection{
		Issues:          map[string]FoldIssue{},
		Comments:        map[string]FoldComment{},
		Labels:          map[FoldLabelKey]FoldElementState{},
		Links:           map[FoldLinkKey]FoldElementState{},
		IssueMetadata:   map[string]json.RawMessage{},
		ProjectMetadata: map[string]json.RawMessage{},
	}
	ordered := append([]FoldEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return compareClock(clockOf(ordered[i]), clockOf(ordered[j])) < 0
	})
	for _, e := range ordered {
		p.apply(e)
	}
	return p
}

func (p *FoldProjection) apply(e FoldEvent) {
	payload := PayloadMap(e.Payload)
	switch e.Type {
	case "issue.created", "issue.snapshot":
		p.applyIssueCreated(e)
	case "issue.updated":
		p.applyIssueUpdated(e, payload)
	case "issue.assigned", "issue.unassigned":
		p.applyOwner(e, payload)
	case "issue.priority_set", "issue.priority_cleared":
		p.applyPriority(e, payload)
	case "issue.closed":
		p.applyClosed(e, payload)
	case "issue.reopened":
		p.applyReopened(e, payload)
	case "issue.soft_deleted":
		p.applyDeleted(e, payload)
	case "issue.restored":
		p.applyRestored(e, payload)
	case "issue.commented":
		p.applyComment(e, payload)
	case "issue.comment_edited":
		p.applyCommentEdited(e, payload)
	case "issue.labeled":
		p.applyLabel(e, payload, true)
	case "issue.unlabeled":
		p.applyLabel(e, payload, false)
	case "issue.linked":
		p.applyLinkEvent(e, payload, true)
	case "issue.unlinked":
		p.applyLinkEvent(e, payload, false)
	case "issue.links_changed":
		p.applyLinksChanged(e, payload)
	case "issue.moved":
		p.applyMoved(e, payload)
	case "issue.metadata_updated":
		uid := issueUID(e, payload)
		if uid != "" {
			p.IssueMetadata[uid] = applyMetadataDiff(p.IssueMetadata[uid], payload["diff"])
			p.touchIssue(uid, issueUpdatedAt(e, payload))
		}
	case "project.metadata_updated":
		if e.ProjectUID != "" {
			p.ProjectMetadata[e.ProjectUID] = applyMetadataDiff(p.ProjectMetadata[e.ProjectUID], payload["diff"])
		}
	case "project.author_rewritten":
		p.applyAuthorRewritten(e, payload)
	case "project.federation_enabled":
		if e.ProjectUID != "" && len(payload["metadata"]) > 0 && string(payload["metadata"]) != "null" {
			p.ProjectMetadata[e.ProjectUID] = canonicalJSON(payload["metadata"])
		}
	case "claim.acquired", "claim.released", "claim.expired", "claim.force_released", "claim.violated":
		// Claim events are federation audit records, not issue projection state.
		return
	}
}

func (p *FoldProjection) applyIssueCreated(e FoldEvent) {
	var in struct {
		UID          string          `json:"uid"`
		ShortID      string          `json:"short_id"`
		Title        string          `json:"title"`
		Body         string          `json:"body"`
		Author       string          `json:"author"`
		Owner        *string         `json:"owner"`
		Priority     *int64          `json:"priority"`
		Status       string          `json:"status"`
		ClosedReason *string         `json:"closed_reason"`
		ClosedAt     *string         `json:"closed_at"`
		DeletedAt    *string         `json:"deleted_at"`
		Metadata     json.RawMessage `json:"metadata"`
		Labels       []string        `json:"labels"`
		Links        []struct {
			Type       string `json:"type"`
			ToIssueUID string `json:"to_issue_uid"`
			Incoming   bool   `json:"incoming"`
			Author     string `json:"author"`
		} `json:"links"`
		Comments []struct {
			CommentUID string `json:"comment_uid"`
			Author     string `json:"author"`
			Body       string `json:"body"`
			CreatedAt  string `json:"created_at"`
		} `json:"comments"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	_ = json.Unmarshal(e.Payload, &in)
	uid := in.UID
	if uid == "" {
		uid = e.IssueUID
	}
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	if issue.UID == "" {
		issue.UID = uid
	}
	if issue.ShortID == "" {
		issue.ShortID = in.ShortID
	}
	if issue.Author == "" {
		issue.Author = in.Author
	}
	if issue.CreatedAt == "" {
		issue.CreatedAt = in.CreatedAt
	}
	if issue.CreatedAt == "" {
		issue.CreatedAt = e.CreatedAt
	}
	if in.UpdatedAt != "" {
		issue.UpdatedAt = in.UpdatedAt
	} else if issue.UpdatedAt == "" {
		issue.UpdatedAt = issue.CreatedAt
	}
	issue.Title = in.Title
	issue.Body = in.Body
	issue.Owner = cloneStringPtr(in.Owner)
	issue.Priority = cloneInt64Ptr(in.Priority)
	issue.Status = in.Status
	issue.ClosedReason = cloneStringPtr(in.ClosedReason)
	issue.ClosedAt = cloneStringPtr(in.ClosedAt)
	issue.DeletedAt = cloneStringPtr(in.DeletedAt)
	issue.ProjectUID = e.ProjectUID
	p.Issues[uid] = issue
	if len(in.Metadata) > 0 && string(in.Metadata) != "null" {
		p.IssueMetadata[uid] = canonicalJSON(in.Metadata)
	}
	for _, label := range in.Labels {
		p.Labels[FoldLabelKey{IssueUID: uid, Label: label}] = FoldElementState{Present: true, Clock: clockOf(e)}
	}
	for _, link := range in.Links {
		if link.ToIssueUID == "" {
			continue
		}
		from, to := uid, link.ToIssueUID
		if link.Incoming && link.Type == "blocks" {
			from, to = to, from
		}
		author := link.Author
		if author == "" {
			author = e.Actor
		}
		p.setLink(from, to, link.Type, true, clockOf(e), author)
	}
	for _, comment := range in.Comments {
		p.setComment(comment.CommentUID, uid, comment.Author, comment.Body, comment.CreatedAt, clockOf(e))
	}
}

func (p *FoldProjection) applyIssueUpdated(e FoldEvent, payload map[string]json.RawMessage) {
	uid := issueUID(e, payload)
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	if v, ok := stringValue(payload["title"]); ok {
		issue.Title = v
	}
	if v, ok := stringValue(payload["body"]); ok {
		issue.Body = v
	}
	if owner, ok := optionalString(payload["owner"]); ok {
		issue.Owner = owner
	}
	if priority, ok := optionalInt64(payload["priority"]); ok {
		issue.Priority = priority
	}
	if v, ok := stringValue(payload["status"]); ok {
		issue.Status = v
	}
	if reason, ok := optionalString(payload["closed_reason"]); ok {
		issue.ClosedReason = reason
	}
	if closedAt, ok := optionalString(payload["closed_at"]); ok {
		issue.ClosedAt = closedAt
	}
	if deletedAt, ok := optionalString(payload["deleted_at"]); ok {
		issue.DeletedAt = deletedAt
	}
	// created_at only ever heals earlier: an import repair carries the recovered
	// genuine creation time, but a later value must never push it forward.
	if v, ok := stringValue(payload["created_at"]); ok && v != "" {
		if issue.CreatedAt == "" || v < issue.CreatedAt {
			issue.CreatedAt = v
		}
	}
	updatedAt := e.CreatedAt
	if v, ok := stringValue(payload["updated_at"]); ok {
		updatedAt = v
	}
	advanceIssueUpdatedAt(&issue, updatedAt)
	p.Issues[uid] = issue
}

func (p *FoldProjection) applyOwner(e FoldEvent, payload map[string]json.RawMessage) {
	uid := issueUID(e, payload)
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	if e.Type == "issue.unassigned" {
		issue.Owner = nil
	} else if owner, ok := optionalString(payload["owner"]); ok {
		issue.Owner = owner
	}
	advanceIssueUpdatedAt(&issue, issueUpdatedAt(e, payload))
	p.Issues[uid] = issue
}

func (p *FoldProjection) applyPriority(e FoldEvent, payload map[string]json.RawMessage) {
	uid := issueUID(e, payload)
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	if e.Type == "issue.priority_cleared" {
		issue.Priority = nil
	} else if prio, ok := int64Value(payload["priority"]); ok {
		issue.Priority = &prio
	}
	advanceIssueUpdatedAt(&issue, issueUpdatedAt(e, payload))
	p.Issues[uid] = issue
}

func (p *FoldProjection) applyClosed(e FoldEvent, payload map[string]json.RawMessage) {
	uid := issueUID(e, payload)
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	issue.Status = "closed"
	if reason, ok := stringValue(payload["reason"]); ok {
		issue.ClosedReason = &reason
	}
	closedAt := e.CreatedAt
	if v, ok := stringValue(payload["closed_at"]); ok {
		closedAt = v
	}
	issue.ClosedAt = &closedAt
	advanceIssueUpdatedAt(&issue, closedAt)
	p.Issues[uid] = issue
}

func (p *FoldProjection) applyReopened(e FoldEvent, payload map[string]json.RawMessage) {
	uid := issueUID(e, payload)
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	issue.Status = "open"
	issue.ClosedReason = nil
	issue.ClosedAt = nil
	advanceIssueUpdatedAt(&issue, issueUpdatedAt(e, payload))
	p.Issues[uid] = issue
}

func (p *FoldProjection) applyDeleted(e FoldEvent, payload map[string]json.RawMessage) {
	uid := issueUID(e, payload)
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	deletedAt := e.CreatedAt
	if v, ok := stringValue(payload["deleted_at"]); ok {
		deletedAt = v
	}
	issue.DeletedAt = &deletedAt
	advanceIssueUpdatedAt(&issue, deletedAt)
	p.Issues[uid] = issue
}

func (p *FoldProjection) applyRestored(e FoldEvent, payload map[string]json.RawMessage) {
	uid := issueUID(e, payload)
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	issue.DeletedAt = nil
	advanceIssueUpdatedAt(&issue, issueUpdatedAt(e, payload))
	p.Issues[uid] = issue
}

func (p *FoldProjection) applyComment(e FoldEvent, payload map[string]json.RawMessage) {
	commentUID, ok := stringValue(payload["comment_uid"])
	if !ok || commentUID == "" {
		return
	}
	author, _ := stringValue(payload["author"])
	body, _ := stringValue(payload["body"])
	createdAt, _ := stringValue(payload["created_at"])
	uid := issueUID(e, payload)
	p.setComment(commentUID, uid, author, body, createdAt, clockOf(e))
	if createdAt == "" {
		createdAt = e.CreatedAt
	}
	p.touchIssue(uid, createdAt)
}

func (p *FoldProjection) applyCommentEdited(e FoldEvent, payload map[string]json.RawMessage) {
	commentUID, ok := stringValue(payload["comment_uid"])
	if !ok || commentUID == "" {
		return
	}
	body, ok := stringValue(payload["body"])
	if !ok {
		return
	}
	uid := issueUID(e, payload)
	p.editCommentBody(commentUID, uid, body, clockOf(e))
	editedAt := e.CreatedAt
	if v, ok := stringValue(payload["edited_at"]); ok && v != "" {
		editedAt = v
	}
	p.touchIssue(uid, editedAt)
}

func (p *FoldProjection) setComment(commentUID, issueUID, author, body, createdAt string, clock FoldClock) {
	if commentUID == "" {
		return
	}
	comment := FoldComment{UID: commentUID, IssueUID: issueUID, Clock: clock}
	comment.Author = author
	comment.Body = body
	comment.CreatedAt = createdAt
	if existing, exists := p.Comments[commentUID]; exists {
		if existing.Author != comment.Author || existing.Body != comment.Body || existing.CreatedAt != comment.CreatedAt {
			p.Warnings = append(p.Warnings, fmt.Sprintf("conflicting duplicate comment %s", commentUID))
		}
		return
	}
	p.Comments[commentUID] = comment
}

func (p *FoldProjection) editCommentBody(commentUID, issueUID, body string, clock FoldClock) {
	if commentUID == "" {
		return
	}
	comment, exists := p.Comments[commentUID]
	if !exists {
		comment = FoldComment{UID: commentUID, IssueUID: issueUID, Clock: clock}
	}
	if comment.IssueUID == "" {
		comment.IssueUID = issueUID
	}
	comment.Body = body
	comment.Clock = clock
	p.Comments[commentUID] = comment
}

func (p *FoldProjection) applyLabel(e FoldEvent, payload map[string]json.RawMessage, present bool) {
	uid := issueUID(e, payload)
	label, ok := stringValue(payload["label"])
	if !ok || uid == "" {
		return
	}
	p.Labels[FoldLabelKey{IssueUID: uid, Label: label}] = FoldElementState{Present: present, Clock: clockOf(e)}
	p.touchIssue(uid, issueUpdatedAt(e, payload))
}

func (p *FoldProjection) applyLinkEvent(e FoldEvent, payload map[string]json.RawMessage, present bool) {
	typ, ok := stringValue(payload["type"])
	if !ok {
		return
	}
	from, fromOK := stringValue(payload["from_uid"])
	to, toOK := stringValue(payload["to_uid"])
	if !fromOK || !toOK {
		return
	}
	p.setLink(from, to, typ, present, clockOf(e), e.Actor)
	p.touchIssue(issueUID(e, payload), issueUpdatedAt(e, payload))
}

func (p *FoldProjection) applyLinksChanged(e FoldEvent, payload map[string]json.RawMessage) {
	base := issueUID(e, payload)
	if base == "" {
		return
	}
	defer p.touchIssue(base, issueUpdatedAt(e, payload))
	p.applyUIDList(base, payload["parent_set_uid"], "parent", true, false, clockOf(e), e.Actor)
	p.applyUIDList(base, payload["parent_removed_uid"], "parent", false, false, clockOf(e), e.Actor)
	for _, uid := range stringSlice(payload["blocks_added_uids"]) {
		p.setLink(base, uid, "blocks", true, clockOf(e), e.Actor)
	}
	for _, uid := range stringSlice(payload["blocks_removed_uids"]) {
		p.setLink(base, uid, "blocks", false, clockOf(e), e.Actor)
	}
	for _, uid := range stringSlice(payload["blocked_by_added_uids"]) {
		p.setLink(uid, base, "blocks", true, clockOf(e), e.Actor)
	}
	for _, uid := range stringSlice(payload["blocked_by_removed_uids"]) {
		p.setLink(uid, base, "blocks", false, clockOf(e), e.Actor)
	}
	for _, uid := range stringSlice(payload["related_added_uids"]) {
		p.setLink(base, uid, "related", true, clockOf(e), e.Actor)
	}
	for _, uid := range stringSlice(payload["related_removed_uids"]) {
		p.setLink(base, uid, "related", false, clockOf(e), e.Actor)
	}
}

func (p *FoldProjection) applyMoved(e FoldEvent, payload map[string]json.RawMessage) {
	uid := issueUID(e, payload)
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	if v, ok := stringValue(payload["to_project_uid"]); ok {
		issue.ProjectUID = v
	}
	if v, ok := stringValue(payload["to_short_id"]); ok {
		issue.ShortID = v
	}
	advanceIssueUpdatedAt(&issue, issueUpdatedAt(e, payload))
	p.Issues[uid] = issue
}

func (p *FoldProjection) applyAuthorRewritten(e FoldEvent, payload map[string]json.RawMessage) {
	from, fromOK := stringValue(payload["from"])
	to, toOK := stringValue(payload["to"])
	if !fromOK || !toOK || from == "" || to == "" {
		return
	}
	for uid, issue := range p.Issues {
		if e.ProjectUID != "" && issue.ProjectUID != e.ProjectUID {
			continue
		}
		changed := false
		if issue.Author == from {
			issue.Author = to
			changed = true
		}
		if issue.Owner != nil && *issue.Owner == from {
			owner := to
			issue.Owner = &owner
			changed = true
		}
		if changed {
			advanceIssueUpdatedAt(&issue, issueUpdatedAt(e, payload))
			p.Issues[uid] = issue
		}
	}
	for uid, comment := range p.Comments {
		if e.ProjectUID != "" {
			issue, ok := p.Issues[comment.IssueUID]
			if !ok || issue.ProjectUID != e.ProjectUID {
				continue
			}
		}
		if comment.Author == from {
			comment.Author = to
			comment.Clock = clockOf(e)
			p.Comments[uid] = comment
		}
	}
	for key, state := range p.Links {
		if e.ProjectUID != "" {
			issue, ok := p.Issues[key.FromUID]
			if !ok || issue.ProjectUID != e.ProjectUID {
				continue
			}
		}
		if state.Author == from {
			state.Author = to
			state.Clock = clockOf(e)
			p.Links[key] = state
		}
	}
}

func (p *FoldProjection) applyUIDList(base string, raw json.RawMessage, typ string, present, incoming bool, clock FoldClock, author string) {
	uid, ok := stringValue(raw)
	if !ok || uid == "" {
		return
	}
	from, to := base, uid
	if incoming {
		from, to = to, from
	}
	p.setLink(from, to, typ, present, clock, author)
}

func (p *FoldProjection) setLink(from, to, typ string, present bool, clock FoldClock, author string) {
	if typ == "related" && from > to {
		from, to = to, from
	}
	if typ == "parent" && present {
		p.clearOlderParents(from, to, clock)
	}
	p.Links[FoldLinkKey{FromUID: from, ToUID: to, Type: typ}] = FoldElementState{Present: present, Clock: clock, Author: author}
}

func (p *FoldProjection) clearOlderParents(childUID, keepParentUID string, clock FoldClock) {
	for key, state := range p.Links {
		if key.Type != "parent" || key.FromUID != childUID || key.ToUID == keepParentUID || !state.Present {
			continue
		}
		if compareClock(state.Clock, clock) > 0 {
			continue
		}
		p.Links[key] = FoldElementState{Present: false, Clock: clock, Author: state.Author}
	}
}

func (p *FoldProjection) ensureIssue(uid string) FoldIssue {
	if issue, ok := p.Issues[uid]; ok {
		return issue
	}
	return FoldIssue{UID: uid}
}

func (p *FoldProjection) touchIssue(uid, updatedAt string) {
	if uid == "" {
		return
	}
	issue := p.ensureIssue(uid)
	advanceIssueUpdatedAt(&issue, updatedAt)
	p.Issues[uid] = issue
}

func advanceIssueUpdatedAt(issue *FoldIssue, updatedAt string) {
	if updatedAt == "" {
		return
	}
	if issue.UpdatedAt == "" || updatedAt > issue.UpdatedAt {
		issue.UpdatedAt = updatedAt
	}
}

func issueUID(e FoldEvent, payload map[string]json.RawMessage) string {
	if e.IssueUID != "" {
		return e.IssueUID
	}
	if v, ok := stringValue(payload["issue_uid"]); ok {
		return v
	}
	if v, ok := stringValue(payload["uid"]); ok {
		return v
	}
	return ""
}

func issueUpdatedAt(e FoldEvent, payload map[string]json.RawMessage) string {
	if updatedAt, ok := stringValue(payload["updated_at"]); ok && updatedAt != "" {
		return updatedAt
	}
	return e.CreatedAt
}

// PayloadMap decodes the event payload bytes into a map of raw JSON fields.
// It returns an empty map on empty input or parse failures; the fold/replay
// paths treat missing fields the same as nil entries.
func PayloadMap(raw json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// StringValue decodes a JSON string field. It returns ok=false when raw is
// empty, JSON null, or not a string.
func StringValue(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	return v, true
}

// stringValue is the internal alias for fold.go's existing callers.
func stringValue(raw json.RawMessage) (string, bool) { return StringValue(raw) }

func optionalString(raw json.RawMessage) (*string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if string(raw) == "null" {
		return nil, true
	}
	v, ok := stringValue(raw)
	if !ok {
		return nil, false
	}
	return &v, true
}

func int64Value(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	return v, true
}

func optionalInt64(raw json.RawMessage) (*int64, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if string(raw) == "null" {
		return nil, true
	}
	v, ok := int64Value(raw)
	if !ok {
		return nil, false
	}
	return &v, true
}

// StringSlice decodes a JSON array of strings. It returns nil on empty input,
// null, or a non-array payload.
func StringSlice(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

// stringSlice is the internal alias for fold.go's existing callers.
func stringSlice(raw json.RawMessage) []string { return StringSlice(raw) }

func applyMetadataDiff(current json.RawMessage, diffRaw json.RawMessage) json.RawMessage {
	var currentMap map[string]json.RawMessage
	if len(current) > 0 && string(current) != "null" {
		_ = json.Unmarshal(current, &currentMap)
	}
	if currentMap == nil {
		currentMap = map[string]json.RawMessage{}
	}
	var diff map[string]struct {
		From json.RawMessage `json:"from"`
		To   json.RawMessage `json:"to"`
	}
	_ = json.Unmarshal(diffRaw, &diff)
	for key, d := range diff {
		if len(d.To) == 0 || string(d.To) == "null" {
			delete(currentMap, key)
			continue
		}
		currentMap[key] = applyMetadataValueDiff(currentMap[key], d.From, d.To)
	}
	out, err := json.Marshal(currentMap)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

func applyMetadataValueDiff(current, from, to json.RawMessage) json.RawMessage {
	if !metadataObjectMergeable(current, from, to) {
		return canonicalJSON(to)
	}
	currentObj := parseRawObject(current)
	fromObj := parseRawObject(from)
	toObj := parseRawObject(to)
	merged := mergeMetadataObject(currentObj, fromObj, toObj)
	out, err := json.Marshal(merged)
	if err != nil {
		return canonicalJSON(to)
	}
	return out
}

func metadataObjectMergeable(current, from, to json.RawMessage) bool {
	if !rawJSONObject(to) {
		return false
	}
	if len(from) > 0 && string(from) != "null" && !rawJSONObject(from) {
		return false
	}
	return len(current) == 0 || string(current) == "null" || rawJSONObject(current)
}

func mergeMetadataObject(
	current map[string]json.RawMessage,
	from map[string]json.RawMessage,
	to map[string]json.RawMessage,
) map[string]json.RawMessage {
	if current == nil {
		current = map[string]json.RawMessage{}
	}
	seen := map[string]struct{}{}
	for key, fromVal := range from {
		seen[key] = struct{}{}
		toVal, ok := to[key]
		if !ok || string(toVal) == "null" {
			delete(current, key)
			continue
		}
		if rawJSONObject(fromVal) && rawJSONObject(toVal) && metadataObjectMergeable(current[key], fromVal, toVal) {
			current[key] = mustMarshalRawObject(mergeMetadataObject(parseRawObject(current[key]), parseRawObject(fromVal), parseRawObject(toVal)))
			continue
		}
		if !jsonRawEqual(fromVal, toVal) {
			current[key] = canonicalJSON(toVal)
		}
	}
	for key, toVal := range to {
		if _, ok := seen[key]; ok {
			continue
		}
		if string(toVal) == "null" {
			delete(current, key)
			continue
		}
		if rawJSONObject(toVal) && metadataObjectMergeable(current[key], nil, toVal) {
			current[key] = mustMarshalRawObject(mergeMetadataObject(parseRawObject(current[key]), nil, parseRawObject(toVal)))
			continue
		}
		current[key] = canonicalJSON(toVal)
	}
	return current
}

func parseRawObject(raw json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if len(raw) == 0 || string(raw) == "null" {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		return map[string]json.RawMessage{}
	}
	return out
}

func rawJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return obj != nil
}

func mustMarshalRawObject(obj map[string]json.RawMessage) json.RawMessage {
	out, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

func jsonRawEqual(a, b json.RawMessage) bool {
	ca, errA := canonicalJSONPreserveNumbers(a)
	cb, errB := canonicalJSONPreserveNumbers(b)
	if errA != nil || errB != nil {
		return string(a) == string(b)
	}
	return string(ca) == string(cb)
}

func canonicalJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	out, err := canonicalJSONPreserveNumbers(raw)
	if err != nil {
		return raw
	}
	return out
}

func clockOf(e FoldEvent) FoldClock {
	return FoldClock{
		HLCPhysicalMS:     e.HLCPhysicalMS,
		HLCCounter:        e.HLCCounter,
		OriginInstanceUID: e.OriginInstanceUID,
		EventUID:          e.UID,
	}
}

func compareClock(a, b FoldClock) int {
	switch {
	case a.HLCPhysicalMS < b.HLCPhysicalMS:
		return -1
	case a.HLCPhysicalMS > b.HLCPhysicalMS:
		return 1
	case a.HLCCounter < b.HLCCounter:
		return -1
	case a.HLCCounter > b.HLCCounter:
		return 1
	case a.OriginInstanceUID < b.OriginInstanceUID:
		return -1
	case a.OriginInstanceUID > b.OriginInstanceUID:
		return 1
	case a.EventUID < b.EventUID:
		return -1
	case a.EventUID > b.EventUID:
		return 1
	default:
		return 0
	}
}

func cloneStringPtr(in *string) *string {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func cloneInt64Ptr(in *int64) *int64 {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}
