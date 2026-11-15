package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// RelayLinkPolicy decides which cross-project link mutations may be folded.
type RelayLinkPolicy struct {
	RootOrigin   bool
	SameRoot     func(string) bool
	CrossProject func(string) bool
}

// StripCrossProjectLinkAdditions excludes links outside the relay's authority.
func StripCrossProjectLinkAdditions(event FoldEvent, policy RelayLinkPolicy) (FoldEvent, bool, error) {
	blocked := func(peer string) bool {
		return policy.CrossProject(peer) && (!policy.RootOrigin || !policy.SameRoot(peer))
	}
	blockedRemoval := func(peer string) bool {
		return !policy.RootOrigin && policy.CrossProject(peer)
	}
	switch event.Type {
	case "issue.linked", "issue.unlinked":
		if event.Type == "issue.unlinked" {
			blocked = blockedRemoval
		}
		payload := PayloadMap(event.Payload)
		for _, key := range []string{"from_uid", "to_uid", "from_issue_uid", "to_issue_uid"} {
			if peer, ok := StringValue(payload[key]); ok && blocked(peer) {
				return event, false, nil
			}
		}
		if event.RelatedIssueUID != "" && blocked(event.RelatedIssueUID) {
			return event, false, nil
		}
		return event, true, nil
	case "issue.created", "issue.snapshot":
		return stripCreationLinks(event, blocked)
	case "issue.links_changed":
		return stripLinksChangedAdditions(event, blocked, blockedRemoval)
	}
	return event, true, nil
}

func stripCreationLinks(event FoldEvent, crossProject func(string) bool) (FoldEvent, bool, error) {
	payload := PayloadMap(event.Payload)
	raw, ok := payload["links"]
	if !ok || string(raw) == "null" {
		return event, true, nil
	}
	var links []jsontext.Value
	if err := json.Unmarshal(raw, &links); err != nil {
		return event, false, fmt.Errorf("decode %s links of %s: %w", event.Type, event.UID, err)
	}
	kept := make([]jsontext.Value, 0, len(links))
	for _, link := range links {
		var peer struct {
			ToIssueUID string `json:"to_issue_uid"`
		}
		if err := json.Unmarshal(link, &peer); err != nil {
			return event, false, fmt.Errorf("decode %s link of %s: %w", event.Type, event.UID, err)
		}
		if crossProject(peer.ToIssueUID) {
			continue
		}
		kept = append(kept, link)
	}
	if len(kept) == len(links) {
		return event, true, nil
	}
	return rewritePayload(event, payload, "links", kept)
}

func stripLinksChangedAdditions(event FoldEvent, crossProject, blockedRemoval func(string) bool) (FoldEvent, bool, error) {
	payload := PayloadMap(event.Payload)
	changed := false
	if parent, ok := StringValue(payload["parent_set_uid"]); ok && crossProject(parent) {
		delete(payload, "parent_set_uid")
		changed = true
	}
	if parent, ok := StringValue(payload["parent_removed_uid"]); ok && blockedRemoval(parent) {
		delete(payload, "parent_removed_uid")
		changed = true
	}
	for _, entry := range []struct {
		key     string
		blocked func(string) bool
	}{
		{"blocks_added_uids", crossProject}, {"blocked_by_added_uids", crossProject}, {"related_added_uids", crossProject},
		{"blocks_removed_uids", blockedRemoval}, {"blocked_by_removed_uids", blockedRemoval}, {"related_removed_uids", blockedRemoval},
	} {
		key := entry.key
		added := StringSlice(payload[key])
		kept := make([]string, 0, len(added))
		for _, peer := range added {
			if !entry.blocked(peer) {
				kept = append(kept, peer)
			}
		}
		if len(kept) == len(added) {
			continue
		}
		encoded, err := json.Marshal(kept)
		if err != nil {
			return event, false, fmt.Errorf("encode %s of %s: %w", key, event.UID, err)
		}
		payload[key] = encoded
		changed = true
	}
	if !changed {
		return event, true, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return event, false, fmt.Errorf("encode links_changed payload of %s: %w", event.UID, err)
	}
	event.Payload = encoded
	return event, true, nil
}

func rewritePayload(event FoldEvent, payload map[string]jsontext.Value, key string, value any) (FoldEvent, bool, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return event, false, fmt.Errorf("encode %s of %s: %w", key, event.UID, err)
	}
	payload[key] = encoded
	if event.Payload, err = json.Marshal(payload); err != nil {
		return event, false, fmt.Errorf("encode %s payload of %s: %w", event.Type, event.UID, err)
	}
	return event, true, nil
}

// ValidateLegacyRelayAdmission rejects fresh crossing additions while preserving
// missing peers and cleanup. Callers resolve membership within the ingest transaction.
func ValidateLegacyRelayAdmission(ctx context.Context, event RemoteEvent, uidCrosses, shortIDCrosses, refCrosses func(context.Context, string) (bool, error)) error {
	var lookupErr error
	crossing := false
	checkUID := func(peer string) bool {
		if peer != "" && lookupErr == nil {
			var restricted bool
			restricted, lookupErr = uidCrosses(ctx, peer)
			crossing = crossing || restricted
		}
		return false // Inspect additions without rewriting the hashed payload.
	}
	switch event.Type {
	case "issue.linked", "issue.links_changed", "issue.created", "issue.snapshot":
		fold := FoldEvent{UID: event.EventUID, Type: event.Type, Payload: event.Payload}
		// Admission inspects additions only; direct-spoke removals remain valid.
		if _, _, err := StripCrossProjectLinkAdditions(fold, RelayLinkPolicy{RootOrigin: true, CrossProject: checkUID, SameRoot: func(string) bool { return false }}); err != nil {
			return err
		}
	case "issue.closed":
		payload := PayloadMap(event.Payload)
		if parent, _ := StringValue(payload["parent_uid"]); parent != "" {
			checkUID(parent) // The UID takes precedence over historical short IDs.
		} else if parent, _ := StringValue(payload["parent_short_id"]); parent != "" {
			restricted, err := shortIDCrosses(ctx, parent)
			if err != nil {
				return err
			}
			crossing = crossing || restricted
		}
		var evidence []Evidence
		if raw, ok := payload["evidence"]; ok {
			if err := json.Unmarshal(raw, &evidence); err != nil {
				return fmt.Errorf("%w: event %s has invalid close evidence: %w", ErrFederationIngestValidation, event.EventUID, err)
			}
		}
		for _, item := range evidence {
			if item.Type != "duplicate-of" && item.Type != "superseded-by" {
				continue
			}
			restricted, err := refCrosses(ctx, item.IssueRef)
			if err != nil {
				return err
			}
			crossing = crossing || restricted
		}
	}
	if lookupErr != nil {
		return lookupErr
	}
	if crossing {
		return fmt.Errorf("%w: event %s: %w", ErrFederationIngestValidation, event.EventUID, ErrFederationCrossProjectBoundary)
	}
	return nil
}

// RelayFoldAuthority holds the group and root of known link endpoints.
type RelayFoldAuthority struct {
	Projects map[string]string
	Roots    map[string]string
	Groups   map[string]map[string]bool
}

// Policy filters one relay event using the endpoint's current membership.
func (a RelayFoldAuthority) Policy(event FoldEvent) RelayLinkPolicy {
	return RelayLinkPolicy{
		RootOrigin:   a.Roots[event.ProjectUID] != "" && a.Roots[event.ProjectUID] == event.OriginInstanceUID,
		CrossProject: func(uid string) bool { return a.Projects[uid] != "" && a.Projects[uid] != event.ProjectUID },
		SameRoot: func(uid string) bool {
			peer := a.Projects[uid]
			return a.Roots[peer] != "" && a.Roots[peer] == a.Roots[event.ProjectUID] && a.Groups[event.ProjectUID][peer]
		},
	}
}

// AllowParentRemoval preserves a root crossing against a replica's edit.
func (a RelayFoldAuthority) AllowParentRemoval(child, parent string, previous, next FoldClock) bool {
	project, peer := a.Projects[child], a.Projects[parent]
	root := a.Roots[project]
	return project == "" || peer == "" || project == peer || root == "" || a.Roots[peer] != root || !a.Groups[project][peer] || previous.OriginInstanceUID != root || next.OriginInstanceUID == root
}
