package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// FederationPayloadIssueUID resolves the issue identity from the envelope and payload.
func FederationPayloadIssueUID(ev RemoteEvent, payload map[string]jsontext.Value) (string, error) {
	var payloadUID string
	if uid, ok := StringValue(payload["issue_uid"]); ok {
		payloadUID = uid
	}
	if uid, ok := StringValue(payload["uid"]); ok {
		if payloadUID != "" && payloadUID != uid {
			return "", fmt.Errorf("%w: payload issue uid disagreement", ErrFederationIngestValidation)
		}
		payloadUID = uid
	}
	if ev.IssueUID != nil {
		if payloadUID != "" && payloadUID != *ev.IssueUID {
			return "", fmt.Errorf("%w: envelope/payload issue uid disagreement", ErrFederationIngestValidation)
		}
		return *ev.IssueUID, nil
	}
	return payloadUID, nil
}

// FederationPayloadReferencedIssueUIDs returns issue references in an event.
func FederationPayloadReferencedIssueUIDs(ev RemoteEvent, payload map[string]jsontext.Value) ([]string, error) {
	var refs []string
	if ev.RelatedIssueUID != nil && *ev.RelatedIssueUID != "" {
		refs = append(refs, *ev.RelatedIssueUID)
	}
	for _, key := range []string{
		"from_uid", "to_uid", "from_issue_uid", "to_issue_uid",
	} {
		if uid, ok := StringValue(payload[key]); ok {
			refs = append(refs, uid)
		}
	}
	changedRefs, err := FederationPayloadLinksChangedIssueUIDs(ev, payload)
	if err != nil {
		return nil, err
	}
	refs = append(refs, changedRefs...)
	links, err := FederationPayloadLinks(ev)
	if err != nil {
		return nil, fmt.Errorf("%w: decode links: %v", ErrFederationIngestValidation, err)
	}
	for _, link := range links {
		if link.ToIssueUID != "" {
			refs = append(refs, link.ToIssueUID)
		}
	}
	return refs, nil
}

// FederationPayloadLinksChangedIssueUIDs returns link changes in an event.
func FederationPayloadLinksChangedIssueUIDs(
	ev RemoteEvent,
	payload map[string]jsontext.Value,
) ([]string, error) {
	if ev.Type != "issue.links_changed" {
		return nil, nil
	}
	var refs []string
	for _, key := range []string{"parent_set_uid", "parent_removed_uid"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		var uid string
		if err := json.Unmarshal(raw, &uid); err != nil {
			return nil, fmt.Errorf("%w: %s must be a string: %v",
				ErrFederationIngestValidation, key, err)
		}
		refs = append(refs, uid)
	}
	for _, key := range []string{
		"blocks_added_uids", "blocks_removed_uids",
		"blocked_by_added_uids", "blocked_by_removed_uids",
		"related_added_uids", "related_removed_uids",
	} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(string(raw)); len(trimmed) == 0 || trimmed[0] != '[' {
			return nil, fmt.Errorf("%w: %s must be an array of strings",
				ErrFederationIngestValidation, key)
		}
		var uids []string
		if err := json.Unmarshal(raw, &uids); err != nil {
			return nil, fmt.Errorf("%w: %s must be an array of strings: %v",
				ErrFederationIngestValidation, key, err)
		}
		refs = append(refs, uids...)
	}
	return refs, nil
}

// FederationPayloadLink is a link embedded in a creation or move snapshot.
type FederationPayloadLink struct {
	Type       string `json:"type"`
	ToIssueUID string `json:"to_issue_uid"`
	Incoming   bool   `json:"incoming"`
}

// FederationPayloadLinks decodes links embedded in an event payload.
func FederationPayloadLinks(ev RemoteEvent) ([]FederationPayloadLink, error) {
	ev = FederationMoveSnapshotEvent(ev)
	var created struct {
		Links []FederationPayloadLink `json:"links"`
	}
	if err := json.Unmarshal(ev.Payload, &created); err != nil {
		return nil, err
	}
	return created.Links, nil
}
