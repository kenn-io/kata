package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/teammate"
	katauid "go.kenn.io/kata/internal/uid"
)

// FederationIssueReferenceMessage is shared by hubs and spokes so recovery
// recognizes the validation message used by released peers.
func FederationIssueReferenceMessage(eventType, issueUID string, existing bool) string {
	if existing {
		return fmt.Sprintf("fresh %s targets existing issue %s", eventType, issueUID)
	}
	return fmt.Sprintf("%s references unknown issue %s", eventType, issueUID)
}

func ParseFederationMissingIssueMessage(message string) string {
	prefix := ErrFederationIngestValidation.Error() + ": "
	message, ok := strings.CutPrefix(message, prefix)
	if !ok {
		return ""
	}
	eventType, issueUID, ok := strings.Cut(message, " references unknown issue ")
	if !ok || eventType == "" || !katauid.Valid(issueUID) {
		return ""
	}
	for _, c := range eventType {
		if (c < 'a' || c > 'z') && c != '.' && c != '_' {
			return ""
		}
	}
	return issueUID
}

// EventTimestampFormat is the millisecond UTC representation covered by portable event hashes.
const EventTimestampFormat = "2006-01-02T15:04:05.000Z"

// ProjectMetadataAdoptionPayload encodes existing project metadata as a
// synthetic from-empty diff for a federation adoption baseline.
func ProjectMetadataAdoptionPayload(metadata JSONBlob) (string, error) {
	current := map[string]jsontext.Value{}
	if len(metadata) > 0 {
		if err := json.Unmarshal([]byte(metadata), &current); err != nil {
			return "", fmt.Errorf("decode adopted project metadata: %w", err)
		}
	}
	type diffEntry struct {
		From any            `json:"from"`
		To   jsontext.Value `json:"to"`
	}
	diff := make(map[string]diffEntry, len(current))
	for key, value := range current {
		diff[key] = diffEntry{From: nil, To: value}
	}
	payload, err := json.Marshal(struct {
		Diff map[string]diffEntry `json:"diff"`
	}{Diff: diff})
	if err != nil {
		return "", fmt.Errorf("marshal adopted project metadata event: %w", err)
	}
	return string(payload), nil
}

// ValidateRemoteEventContentHash canonicalizes an incoming payload and verifies
// the portable event hash before a backend persists it.
func ValidateRemoteEventContentHash(event RemoteEvent) (jsontext.Value, string, error) {
	expectedHash, err := RemoteEventContentHash(event)
	if err != nil {
		return nil, "", err
	}
	if !strings.EqualFold(expectedHash, event.ContentHash) {
		return nil, "", fmt.Errorf("%w: event %s", ErrRemoteEventHashMismatch, event.EventUID)
	}
	return remoteEventPayload(event), remoteEventCreatedAt(event), nil
}

// RemoteEventContentHash computes the content hash the portable event fields
// commit to, ignoring the hash the event advertises.
func RemoteEventContentHash(event RemoteEvent) (string, error) {
	hash, err := EventContentHash(EventHashInput{
		UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter, CreatedAt: remoteEventCreatedAt(event), Payload: remoteEventPayload(event),
	})
	if err != nil {
		return "", fmt.Errorf("remote event content hash: %w", err)
	}
	return hash, nil
}

func remoteEventPayload(event RemoteEvent) jsontext.Value {
	if len(event.Payload) == 0 {
		return jsontext.Value(`{}`)
	}
	return event.Payload
}

func remoteEventCreatedAt(event RemoteEvent) string {
	return event.CreatedAt.UTC().Format(EventTimestampFormat)
}

// ValidateFederationEntries checks comment teammate attribution and requires
// embedded comments and links to be JSON objects on created and snapshot events.
// Ingest and pull paths must call it before persisting the event.
func ValidateFederationEntries(eventType, eventUID string, payloadJSON jsontext.Value) error {
	if err := validateFederationPlanningZones(eventType, eventUID, payloadJSON); err != nil {
		return err
	}
	return validateFederationEntries(eventType, eventUID, payloadJSON)
}

func validateFederationPlanningZones(eventType, eventUID string, payloadJSON jsontext.Value) error {
	switch eventType {
	case "issue.created", "issue.snapshot", "issue.metadata_updated", RepeatedImportEventType, "projection.issue":
	default:
		return nil
	}
	var payload struct {
		Metadata map[string]jsontext.Value `json:"metadata"`
		Diff     map[string]struct {
			To jsontext.Value `json:"to"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil
	}
	for _, key := range []string{"scheduled_on", "deadline_on"} {
		for _, raw := range []jsontext.Value{payload.Metadata[key], payload.Diff[key].To} {
			var value string
			if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
				continue
			}
			if _, zone := metadata.SplitScheduleZone(value); zone != "" {
				if err := metadata.Validate(metadata.IssueRegistry, key, raw); err != nil {
					return fmt.Errorf("%w: event %s %s: %v", ErrFederationIngestValidation, eventUID, key, err)
				}
			}
		}
	}
	return nil
}

func validateFederationEntries(eventType, eventUID string, payloadJSON jsontext.Value) error {
	if eventType == "issue.moved" {
		snapshot := PayloadMap(payloadJSON)["snapshot"]
		// Historical local moves have no snapshot; federation ingest separately
		// requires one through FederationMoveProjects.
		if len(snapshot) == 0 {
			return nil
		}
		return ValidateFederationEntries("issue.snapshot", eventUID, snapshot)
	}
	if eventType == "issue.commented" || eventType == "issue.comment_edited" {
		var payload map[string]jsontext.Value
		if err := json.Unmarshal(payloadJSON, &payload); err != nil {
			return fmt.Errorf("%w: event %s %s payload is invalid JSON",
				ErrFederationIngestValidation, eventUID, eventType)
		}
		if err := validateCommentEntry(payload); err != nil {
			return fmt.Errorf("%w: event %s %s: %v", ErrFederationIngestValidation, eventUID, eventType, err)
		}
		return validateFederationTeammate(payload["teammate"], eventUID, eventType)
	}
	// A repeated import event carries a whole issue's comments, as a snapshot
	// does.
	if eventType != "issue.snapshot" && eventType != "issue.created" && eventType != RepeatedImportEventType {
		return nil
	}
	var payload struct {
		Comments []map[string]jsontext.Value `json:"comments"`
		Links    []map[string]jsontext.Value `json:"links"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return fmt.Errorf("%w: event %s %s entries are invalid JSON",
			ErrFederationIngestValidation, eventUID, eventType)
	}
	for field, entries := range map[string][]map[string]jsontext.Value{
		"comments": payload.Comments, "links": payload.Links,
	} {
		for _, entry := range entries {
			if entry == nil {
				return fmt.Errorf("%w: event %s %s %s payload entry must be a JSON object",
					ErrFederationIngestValidation, eventUID, eventType, field)
			}
			if field == "comments" {
				if err := validateCommentEntry(entry); err != nil {
					return fmt.Errorf("%w: event %s %s: %v", ErrFederationIngestValidation, eventUID, eventType, err)
				}
				if err := validateFederationTeammate(entry["teammate"], eventUID, eventType); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateFederationTeammate(raw jsontext.Value, eventUID, eventType string) error {
	if len(raw) == 0 {
		return nil
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%w: event %s %s teammate must be a string",
			ErrFederationIngestValidation, eventUID, eventType)
	}
	if value == nil {
		return nil
	}
	if err := teammate.Validate(*value); err != nil {
		return fmt.Errorf("%w: event %s %s: %v",
			ErrFederationIngestValidation, eventUID, eventType, err)
	}
	return nil
}

// RetainedSnapshotLocalLinks omits cross-project links that cannot be sent as
// part of recovery. Their original events and local projections stay intact.
func RetainedSnapshotLocalLinks(ctx context.Context, tx *sql.Tx, issue Issue, payload string) (string, error) {
	var document map[string]jsontext.Value
	if err := json.Unmarshal([]byte(payload), &document); err != nil {
		return "", err
	}
	raw, ok := document["links"]
	if !ok {
		return payload, nil
	}
	var links []IssueDocumentLink
	if err := json.Unmarshal(raw, &links); err != nil {
		return "", err
	}
	if len(links) == 0 {
		return payload, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT peer.uid FROM links l JOIN issues peer ON peer.id = CASE WHEN l.from_issue_id = $1 THEN l.to_issue_id ELSE l.from_issue_id END WHERE (l.from_issue_id = $1 OR l.to_issue_id = $1) AND peer.project_id = $2`, issue.ID, issue.ProjectID)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	local := make(map[string]bool)
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return "", err
		}
		local[uid] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	links = slices.DeleteFunc(links, func(link IssueDocumentLink) bool { return !local[link.ToIssueUID] })
	document["links"], err = json.Marshal(links)
	if err != nil {
		return "", err
	}
	result, err := json.Marshal(document)
	return string(result), err
}

// CanonicalizeFederationSnapshotAuthors rewrites an adoption snapshot's
// embedded authors to its bound actor and recomputes the portable hash.
func CanonicalizeFederationSnapshotAuthors(event RemoteEvent, boundActor string) (RemoteEvent, error) {
	boundActor = strings.TrimSpace(boundActor)
	if (event.Type != "issue.snapshot" && event.Type != "cron.job.snapshot" && event.Type != "cron.workflow.snapshot") || boundActor == "" {
		return event, nil
	}
	var payload map[string]jsontext.Value
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot payload is invalid JSON",
			ErrFederationIngestValidation, event.EventUID)
	}
	if payload == nil {
		return RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot payload must be a JSON object",
			ErrFederationIngestValidation, event.EventUID)
	}
	actorJSON, err := json.Marshal(boundActor)
	if err != nil {
		return RemoteEvent{}, fmt.Errorf("marshal federation snapshot author: %w", err)
	}
	payload["author"] = actorJSON
	for _, field := range []string{"comments", "links"} {
		raw, ok := payload[field]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var entries []map[string]jsontext.Value
		if err := json.Unmarshal(raw, &entries); err != nil {
			return RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot %s payload is invalid JSON",
				ErrFederationIngestValidation, event.EventUID, field)
		}
		for index := range entries {
			if entries[index] == nil {
				return RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot %s payload entry must be a JSON object",
					ErrFederationIngestValidation, event.EventUID, field)
			}
			entries[index]["author"] = actorJSON
		}
		normalized, err := json.Marshal(entries)
		if err != nil {
			return RemoteEvent{}, fmt.Errorf("marshal federation snapshot %s: %w", field, err)
		}
		payload[field] = normalized
	}
	normalizedPayload, err := json.Marshal(payload)
	if err != nil {
		return RemoteEvent{}, fmt.Errorf("marshal federation snapshot payload: %w", err)
	}
	event.Payload = normalizedPayload
	event.ContentHash, err = EventContentHash(EventHashInput{
		UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter,
		CreatedAt:  event.CreatedAt.UTC().Format(EventTimestampFormat), Payload: event.Payload,
	})
	if err != nil {
		return RemoteEvent{}, fmt.Errorf("hash canonical federation snapshot: %w", err)
	}
	return event, nil
}

// LocalEchoMatchesCanonicalSnapshot reports whether a pulled hash mismatch is
// exactly the allowed canonical-author rewrite of one local adoption snapshot.
func LocalEchoMatchesCanonicalSnapshot(existing Event, remote RemoteEvent) (bool, error) {
	if existing.Type != remote.Type || (existing.Type != "issue.snapshot" && existing.Type != "cron.job.snapshot" && existing.Type != "cron.workflow.snapshot") {
		return false, nil
	}
	if existing.UID != remote.EventUID ||
		existing.OriginInstanceUID != remote.OriginInstanceUID ||
		existing.ProjectUID != remote.ProjectUID ||
		!stringPointersEqual(existing.IssueUID, remote.IssueUID) ||
		!stringPointersEqual(existing.RelatedIssueUID, remote.RelatedIssueUID) ||
		existing.Actor != remote.Actor ||
		existing.HLCPhysicalMS != remote.HLCPhysicalMS ||
		existing.HLCCounter != remote.HLCCounter ||
		existing.CreatedAt.UTC().Format(EventTimestampFormat) != remote.CreatedAt.UTC().Format(EventTimestampFormat) {
		return false, nil
	}
	local := RemoteEvent{
		EventUID: existing.UID, OriginInstanceUID: existing.OriginInstanceUID,
		ProjectUID: existing.ProjectUID, ProjectName: existing.ProjectName,
		IssueUID: existing.IssueUID, RelatedIssueUID: existing.RelatedIssueUID,
		Type: existing.Type, Actor: existing.Actor, HLCPhysicalMS: existing.HLCPhysicalMS,
		HLCCounter: existing.HLCCounter, ContentHash: existing.ContentHash,
		Payload: jsontext.Value(existing.Payload), CreatedAt: existing.CreatedAt,
	}
	canonical, err := CanonicalizeFederationSnapshotAuthors(local, remote.Actor)
	if err != nil {
		return false, err
	}
	return canonical.ContentHash == remote.ContentHash, nil
}

func stringPointersEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// DirectLinkPeers returns explicit link endpoints eligible for
// a supplemental direct-spoke proof. It never modifies the event payload.
func DirectLinkPeers(event FoldEvent) (map[string]bool, error) {
	if event.Type != "issue.unlinked" && !directLinkAdditionEligible(event) {
		return nil, nil
	}
	var peers map[string]bool
	add := func(uid string) {
		if uid != "" {
			if peers == nil {
				peers = make(map[string]bool)
			}
			peers[uid] = true
		}
	}
	_, _, err := StripCrossProjectLinkAdditions(event, func(uid string) bool {
		add(uid)
		return false
	})
	if event.Type == "issue.links_changed" || event.Type == "issue.unlinked" {
		payload := PayloadMap(event.Payload)
		if event.Type == "issue.links_changed" {
			uid, _ := StringValue(payload["parent_removed_uid"])
			add(uid)
			for _, key := range []string{"blocks_removed_uids", "blocked_by_removed_uids", "related_removed_uids"} {
				for _, uid := range StringSlice(payload[key]) {
					delete(peers, uid)
				}
			}
		} else if kind, _ := StringValue(payload["type"]); kind == "parent" {
			child, parent := parentRemovalEndpoints(event, payload)
			add(map[string]string{child: parent, parent: child}[event.IssueUID])
		}
	}
	return peers, err
}

// directLinkAdditionEligible requires a parent link's primary issue to be the
// child. A parent-primary event needs ordinary authority for the child.
func directLinkAdditionEligible(event FoldEvent) bool {
	switch event.Type {
	case "issue.created", "issue.links_changed":
		return true
	case "issue.linked":
	default:
		return false
	}
	payload := PayloadMap(event.Payload)
	if kind, _ := StringValue(payload["type"]); kind != "parent" {
		return true
	}
	child, _ := StringValue(payload["from_uid"])
	return child != "" && child == event.IssueUID
}

// DirectParentRemovals tracks parent operations only when needed for
// reference authorization or validation of an older child-primary unlink.
func DirectParentRemovals(ctx context.Context, tx federationHistoryQuery, events []RemoteEvent, authorizeReference bool) (map[string][]string, error) {
	if !authorizeReference {
		needed := false
		for _, event := range events {
			if event.Type != "issue.unlinked" {
				continue
			}
			payload := PayloadMap(event.Payload)
			_, hasFrom := payload["link_from_uid"]
			_, hasTo := payload["link_to_uid"]
			child, parent := parentRemovalEndpoints(moveSnapshotFoldEvent(event), payload)
			if !hasFrom && !hasTo && child != "" && parent != "" {
				needed = true
				break
			}
		}
		if !needed {
			return nil, nil
		}
	}
	ordered := make([]FoldEvent, 0, len(events))
	for _, event := range events {
		ordered = append(ordered, moveSnapshotFoldEvent(event))
	}
	slices.SortStableFunc(ordered, func(a, b FoldEvent) int { return compareClock(clockOf(a), clockOf(b)) })
	parents := map[string]string{}
	removed := map[string][]string{}
	parentOf := func(child string) (string, error) {
		if parent, known := parents[child]; known {
			return parent, nil
		}
		var parent string
		err := tx.QueryRowContext(ctx, `SELECT to_issue_uid FROM links WHERE from_issue_uid=$1 AND type='parent'`, child).Scan(&parent)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		parents[child] = parent
		return parent, nil
	}
	for _, event := range ordered {
		if event.Type == "issue.unlinked" || event.Type == "issue.links_changed" {
			from, to := parentRemovalEndpoints(event, PayloadMap(event.Payload))
			if from != "" && to != "" {
				parent, err := parentOf(from)
				if err != nil {
					return nil, err
				}
				_, hasFrom := PayloadMap(event.Payload)["link_from_uid"]
				_, hasTo := PayloadMap(event.Payload)["link_to_uid"]
				if event.Type == "issue.unlinked" && !hasFrom && !hasTo && parent != to {
					return nil, fmt.Errorf("%w: This older change tries to remove a parent the issue no longer has", ErrFederationIngestValidation)
				}
				if parent == to {
					parents[from] = ""
				}
			}
		}
		assignments, err := directParentAssignments(event)
		if err != nil {
			return nil, err
		}
		for _, key := range assignments {
			parent, err := parentOf(key.FromUID)
			if err != nil {
				return nil, err
			}
			if parent != "" && parent != key.ToUID {
				removed[event.UID] = append(removed[event.UID], parent)
			}
			parents[key.FromUID] = key.ToUID
		}
	}
	return removed, nil
}

// parentRemovalEndpoints returns the child-to-parent storage endpoints removed
// by event. Old parent unlinks without storage endpoints are read as
// from_uid -> to_uid only when the event's primary issue is the child.
func parentRemovalEndpoints(event FoldEvent, payload map[string]jsontext.Value) (string, string) {
	if event.Type == "issue.links_changed" {
		to, _ := StringValue(payload["parent_removed_uid"])
		return event.IssueUID, to
	}
	if kind, _ := StringValue(payload["type"]); kind != "parent" {
		return "", ""
	}
	_, fromPresent := payload["link_from_uid"]
	_, toPresent := payload["link_to_uid"]
	if !fromPresent && !toPresent {
		from, _ := StringValue(payload["from_uid"])
		to, _ := StringValue(payload["to_uid"])
		if from != event.IssueUID {
			return "", ""
		}
		return from, to
	}
	from, _ := StringValue(payload["link_from_uid"])
	to, _ := StringValue(payload["link_to_uid"])
	return from, to
}

// ValidateUnlinkStorageEndpoints checks the optional directional storage
// endpoints of an issue.unlinked event. Blocks unlinks must carry them. Parent
// unlinks may omit both only in the old form whose primary issue is the
// payload from_uid (the child). The payload is never rewritten.
func ValidateUnlinkStorageEndpoints(
	event RemoteEvent,
	payload map[string]jsontext.Value,
	linkType, primaryUID, fromUID, toUID string,
) error {
	if event.Type != "issue.unlinked" {
		return nil
	}
	rawFrom, fromPresent := payload["link_from_uid"]
	rawTo, toPresent := payload["link_to_uid"]
	if fromPresent != toPresent {
		return fmt.Errorf("%w: issue.unlinked storage endpoints must be paired", ErrFederationIngestValidation)
	}
	if !fromPresent {
		if linkType == "blocks" || (linkType == "parent" && primaryUID != fromUID) {
			return fmt.Errorf("%w: issue.unlinked missing directional storage endpoints", ErrFederationIngestValidation)
		}
		return nil
	}
	linkFromUID, fromOK := StringValue(rawFrom)
	linkToUID, toOK := StringValue(rawTo)
	if !fromOK || !toOK {
		return fmt.Errorf("%w: issue.unlinked storage endpoints must be strings", ErrFederationIngestValidation)
	}
	if !katauid.Valid(linkFromUID) || !katauid.Valid(linkToUID) || linkFromUID == linkToUID {
		return fmt.Errorf("%w: issue.unlinked has invalid storage endpoints", ErrFederationIngestValidation)
	}
	matchesForward := linkFromUID == fromUID && linkToUID == toUID
	matchesReverse := linkFromUID == toUID && linkToUID == fromUID
	if !matchesForward && !matchesReverse {
		return fmt.Errorf("%w: issue.unlinked storage endpoints disagree with payload endpoints", ErrFederationIngestValidation)
	}
	return nil
}

func directParentAssignments(event FoldEvent) ([]FoldLinkKey, error) {
	switch event.Type {
	case "issue.links_changed":
		parent, _ := StringValue(PayloadMap(event.Payload)["parent_set_uid"])
		if parent != "" {
			return []FoldLinkKey{{FromUID: event.IssueUID, ToUID: parent, Type: "parent"}}, nil
		}
	case "issue.linked":
		payload := PayloadMap(event.Payload)
		kind, _ := StringValue(payload["type"])
		if kind == "parent" {
			from, _ := StringValue(payload["from_uid"])
			to, _ := StringValue(payload["to_uid"])
			if from != "" && to != "" {
				return []FoldLinkKey{{FromUID: from, ToUID: to, Type: kind}}, nil
			}
		}
	case "issue.created", "issue.snapshot":
		var doc struct {
			Links []IssueDocumentLink `json:"links"`
		}
		if err := json.Unmarshal(event.Payload, &doc); err != nil {
			return nil, fmt.Errorf("%w: decode parent links: %v", ErrFederationIngestValidation, err)
		}
		var parents []FoldLinkKey
		for _, link := range doc.Links {
			if link.Type == "parent" && link.ToIssueUID != "" {
				parents = append(parents, snapshotLinkKey(event.IssueUID, link))
			}
		}
		return parents, nil
	}
	return nil, nil
}
