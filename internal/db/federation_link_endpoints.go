package db

import (
	"encoding/json"
	"sort"
)

// FederationEventLinkEndpointUIDs returns every issue UID an event names as a
// link endpoint, sorted and deduplicated.
//
// Link reconciliation needs to know which projects a batch can touch before it
// folds anything. The endpoints cannot be read from indexed columns alone:
// issue.created and issue.snapshot carry their links inside the payload, and
// issue.links_changed names peers in per-relation UID lists. Both are only
// visible by parsing the payload, which is cheap for a batch held in memory and
// is the reason the scope is computed at ingest rather than from stored rows.
//
// The key sets mirror FoldProjection.apply exactly. A key added there without
// being added here would let a link's peer project fall outside the reconciled
// scope, so the two must be kept in step.
func FederationEventLinkEndpointUIDs(event RemoteEvent) []string {
	if !FederationEventAffectsLinks(event.Type) {
		return nil
	}
	seen := map[string]struct{}{}
	add := func(uid string) {
		if uid != "" {
			seen[uid] = struct{}{}
		}
	}
	if event.IssueUID != nil {
		add(*event.IssueUID)
	}
	if event.RelatedIssueUID != nil {
		add(*event.RelatedIssueUID)
	}
	payload := PayloadMap(event.Payload)
	addKey := func(key string) {
		if value, ok := StringValue(payload[key]); ok {
			add(value)
		}
	}
	addList := func(key string) {
		var uids []string
		if raw, ok := payload[key]; ok {
			_ = json.Unmarshal(raw, &uids)
		}
		for _, uid := range uids {
			add(uid)
		}
	}

	switch event.Type {
	case "issue.created", "issue.snapshot":
		var body struct {
			UID   string `json:"uid"`
			Links []struct {
				ToIssueUID string `json:"to_issue_uid"`
			} `json:"links"`
		}
		_ = json.Unmarshal(event.Payload, &body)
		add(body.UID)
		for _, link := range body.Links {
			add(link.ToIssueUID)
		}
	case "issue.linked", "issue.unlinked":
		for _, key := range []string{"issue_uid", "from_uid", "to_uid", "link_from_uid", "link_to_uid"} {
			addKey(key)
		}
	case "issue.links_changed":
		for _, key := range []string{"issue_uid", "parent_set_uid", "parent_removed_uid"} {
			addKey(key)
		}
		for _, key := range []string{
			"blocks_added_uids", "blocks_removed_uids",
			"blocked_by_added_uids", "blocked_by_removed_uids",
			"related_added_uids", "related_removed_uids",
		} {
			addList(key)
		}
	default:
		// issue.moved, issue.soft_deleted, issue.restored and
		// project.author_rewritten change endpoint resolution or link authorship
		// for issues already covered by the columns handled above.
		addKey("issue_uid")
	}

	out := make([]string, 0, len(seen))
	for uid := range seen {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}
