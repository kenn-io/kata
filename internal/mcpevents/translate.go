package mcpevents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/kata/internal/notification"
)

// inboxSourceEventID is the eventId of an occurrence for one recipient. It is
// stable across retries and restarts.
func inboxSourceEventID(daemonUID, eventUID, recipient string) string {
	data, _ := json.Marshal([]string{daemonUID, eventUID, EventName, recipient})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// inboxEventReason classifies a visible event for one subscription. It
// returns "notification" when the recipient's inbox entry changed,
// "visibility" when a related issue or project changed, and "" otherwise.
func inboxEventReason(o Occurrence, args Arguments) string {
	if args.IssueUID != "" && strings.HasPrefix(o.Type, "issue.") && o.IssueUID != args.IssueUID {
		return ""
	}
	switch o.Type {
	case "issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored", "issue.moved", "project.removed", "project.restored", "project.merged", "project.renamed":
		return "visibility"
	case "issue.created", "issue.snapshot", "issue.metadata_updated", "issue.updated", "issue.linked", "issue.unlinked", "issue.links_changed":
	default:
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(o.Payload, &payload) != nil {
		return "visibility"
	}
	key := notification.MetadataKey(args.For)
	switch o.Type {
	case "issue.linked", "issue.unlinked":
		// Parent changes can expose requests whose original notification event
		// was already consumed while its issue was outside a subtree grant.
		if payload["type"] == "parent" {
			return "visibility"
		}
	case "issue.links_changed":
		for _, field := range []string{"parent_set_uid", "parent_removed_uid"} {
			if _, present := payload[field]; present {
				return "visibility"
			}
		}
	case "issue.created", "issue.snapshot":
		metadata, _ := payload["metadata"].(map[string]any)
		if _, ok := notification.ParseValue(metadata[key]); ok {
			return "notification"
		}
		if o.Type == "issue.snapshot" {
			return "visibility"
		}
	case "issue.metadata_updated":
		diff, ok := payload["diff"].(map[string]any)
		if !ok {
			return "visibility"
		}
		change, present := diff[key]
		if !present {
			return ""
		}
		values, ok := change.(map[string]any)
		if !ok {
			return "visibility"
		}
		_, from := notification.ParseValue(values["from"])
		_, to := notification.ParseValue(values["to"])
		if from || to {
			return "notification"
		}
	case "issue.updated":
		for _, field := range []string{"status", "deleted_at", "closed_at"} {
			if _, ok := payload[field]; ok {
				return "visibility"
			}
		}
	}
	return ""
}
