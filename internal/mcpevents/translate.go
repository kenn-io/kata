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

// inboxVisibilityTypes always wake a matching subscription as visibility.
var inboxVisibilityTypes = map[string]bool{
	"issue.closed": true, "issue.reopened": true, "issue.soft_deleted": true, "issue.restored": true, "issue.moved": true,
	"project.removed": true, "project.restored": true, "project.merged": true, "project.renamed": true,
}

// inboxRequestTypes wake a subscription only when their payload touches the
// recipient's request or an issue's parent.
var inboxRequestTypes = map[string]bool{
	"issue.created": true, "issue.snapshot": true, "issue.metadata_updated": true, "issue.updated": true,
	"issue.linked": true, "issue.unlinked": true, "issue.links_changed": true,
}

// issueLifecycle reports an occurrence that changes whether an issue is open
// or deleted. It removes or restores an inbox entry only for an issue that
// carries the recipient's request; Scan marks it for that check.
func issueLifecycle(o Occurrence) bool {
	switch o.Type {
	case "issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored":
		return true
	case "issue.updated":
		var payload map[string]any
		if json.Unmarshal(o.Payload, &payload) != nil {
			return false
		}
		for _, field := range []string{"status", "deleted_at", "closed_at"} {
			if _, ok := payload[field]; ok {
				return true
			}
		}
	}
	return false
}

// inboxEventReason classifies a visible event for one subscription. It
// returns "notification" when the recipient's inbox entry changed,
// "visibility" when a related issue or project changed, and "" otherwise.
func inboxEventReason(o Occurrence, args Arguments) string {
	if args.IssueUID != "" && strings.HasPrefix(o.Type, "issue.") && o.IssueUID != args.IssueUID {
		return ""
	}
	switch {
	case inboxVisibilityTypes[o.Type]:
		return "visibility"
	case !inboxRequestTypes[o.Type]:
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
