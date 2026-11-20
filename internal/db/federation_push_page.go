package db

import (
	"context"
	"encoding/json/jsontext"
)

// legacyParentEndpoints returns the payload child and parent of a parent link
// event whose primary issue is the child.
func legacyParentEndpoints(event Event, eventType string) (string, string, bool) {
	payload := PayloadMap(jsontext.Value(event.Payload))
	kind, _ := StringValue(payload["type"])
	from, _ := StringValue(payload["from_uid"])
	to, _ := StringValue(payload["to_uid"])
	ok := event.Type == eventType && kind == "parent" && event.IssueUID != nil && *event.IssueUID == from && to != ""
	return from, to, ok
}

// IsLegacyParentUnlink reports whether event is a parent unlink that carries
// no link_from_uid/link_to_uid endpoints.
func IsLegacyParentUnlink(event Event) bool {
	if _, _, ok := legacyParentEndpoints(event, "issue.unlinked"); !ok {
		return false
	}
	payload := PayloadMap(jsontext.Value(event.Payload))
	return payload["link_from_uid"] == nil && payload["link_to_uid"] == nil
}

// IsLegacyParentReplacement reports whether link is the child-primary parent
// link that replaces the parent removed by unlink.
func IsLegacyParentReplacement(unlink, link Event) bool {
	child, parent, ok := legacyParentEndpoints(unlink, "issue.unlinked")
	laterChild, laterParent, laterOK := legacyParentEndpoints(link, "issue.linked")
	return ok && laterOK && laterChild == child && laterParent != parent
}

// KeepLegacyParentReplacements keeps legacy unlinks with their later replacement
// links, preserving the order of events between them.
func KeepLegacyParentReplacements(ctx context.Context, events []Event, fetch func(context.Context, int64, int) ([]Event, error)) ([]Event, error) {
	var pending []int
	for i, event := range events {
		if !IsLegacyParentUnlink(event) {
			continue
		}
		found := false
		for _, later := range events[i+1:] {
			if IsLegacyParentReplacement(event, later) {
				found = true
				break
			}
		}
		if !found {
			pending = append(pending, i)
		}
	}
	if len(pending) == 0 {
		return events, nil
	}
	// Moving the cut to index i leaves room for i later events on the next
	// page. A page starting with an unlink can extend by at most 1000 events.
	limit := pending[len(pending)-1]
	if pending[0] == 0 {
		limit = max(limit, 1000)
	}
	next, err := fetch(ctx, events[len(events)-1].ID, limit)
	if err != nil {
		return nil, err
	}
	for _, i := range pending {
		lookahead := i
		if i == 0 {
			lookahead = 1000
		}
		for j, later := range next[:min(lookahead, len(next))] {
			if IsLegacyParentReplacement(events[i], later) {
				if i == 0 {
					return append(events, next[:j+1]...), nil
				}
				return events[:i], nil
			}
		}
	}
	return events, nil
}
