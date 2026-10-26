package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/shortid"
)

type relayForwardPathKey struct{}

// WithRelayForwardPath carries an already validated ingress path only within
// the storage transaction. It is routing state, never an authorization grant.
func WithRelayForwardPath(ctx context.Context, path []string) context.Context {
	return context.WithValue(ctx, relayForwardPathKey{}, slices.Clone(path))
}

// RelayForwardPath returns a copy of the validated ingress routing path.
func RelayForwardPath(ctx context.Context) []string {
	path, _ := ctx.Value(relayForwardPathKey{}).([]string)
	return slices.Clone(path)
}

// Checkpoint capture and owner replay retain existing durable state without
// creating new ordinary deliveries. Only native snapshot/replay paths set this;
// no request field or transported actor label can establish it.
type relayStateCaptureKey struct{}

type relayCheckpointCaptureKey struct{}

// WithRelayStateCapture marks native checkpoint or owner replay without new delivery.
func WithRelayStateCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, relayStateCaptureKey{}, true)
}

// WithRelayCheckpointCapture marks a fresh root checkpoint capture. It also
// suppresses ordinary relay deliveries while preserving the more specific
// checkpoint marker for snapshot encoding.
func WithRelayCheckpointCapture(ctx context.Context) context.Context {
	return context.WithValue(WithRelayStateCapture(ctx), relayCheckpointCaptureKey{}, true)
}

// RelayStateCapture reports the native checkpoint or owner replay marker.
func RelayStateCapture(ctx context.Context) bool {
	capture, _ := ctx.Value(relayStateCaptureKey{}).(bool)
	return capture
}

// RelayCheckpointCapture reports whether the caller is encoding a new root
// checkpoint, rather than replaying other retained owner state.
func RelayCheckpointCapture(ctx context.Context) bool {
	capture, _ := ctx.Value(relayCheckpointCaptureKey{}).(bool)
	return capture
}

type relayArtifactSourceKey struct{}

// WithRelayArtifactSourceUID retains a validated incoming artifact offer's
// identity during forwarding. This is routing state, never project authority.
func WithRelayArtifactSourceUID(ctx context.Context, sourceUID string) context.Context {
	return context.WithValue(ctx, relayArtifactSourceKey{}, sourceUID)
}

// RelayArtifactSourceUID returns the validated offer identity being forwarded.
func RelayArtifactSourceUID(ctx context.Context) string {
	sourceUID, _ := ctx.Value(relayArtifactSourceKey{}).(string)
	return sourceUID
}

// RelayEventReferencedProjects discovers only structured native references.
func RelayEventReferencedProjects(event RemoteEvent, refs []string, lookup *EventIssueLookup, commentIssue func(string, string) (Issue, error), shortIssue func(string) (Issue, error)) ([]int64, error) {
	projects := []int64{}
	record := func(issue Issue, err error) error {
		if err != nil {
			return err
		}
		if !slices.Contains(projects, issue.ProjectID) {
			projects = append(projects, issue.ProjectID)
		}
		return nil
	}
	refs = slices.Clone(refs)
	if event.IssueUID != nil {
		refs = append(refs, *event.IssueUID)
	}
	if event.RelatedIssueUID != nil {
		refs = append(refs, *event.RelatedIssueUID)
	}
	var payload struct {
		ReplyToUID *string                   `json:"reply_to_uid"`
		CommentUID string                    `json:"comment_uid"`
		Metadata   map[string]jsontext.Value `json:"metadata"`
		Diff       map[string]struct {
			From jsontext.Value `json:"from"`
			To   jsontext.Value `json:"to"`
		} `json:"diff"`
		Comments []struct {
			CommentUID string  `json:"comment_uid"`
			ReplyToUID *string `json:"reply_to_uid"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, errors.Join(ErrNotFound, err)
	}
	if event.Type == "issue.closed" {
		closed, err := decodeCloseEventReferences(event.Payload)
		if err != nil {
			return nil, errors.Join(ErrNotFound, err)
		}
		fields := PayloadMap(event.Payload)
		for _, key := range []string{"parent_uid", "parent_short_id"} {
			if raw, exists := fields[key]; exists && string(raw) == "null" {
				return nil, ErrNotFound
			}
		}
		if _, exists := fields["evidence"]; exists && closed.Evidence == nil {
			return nil, ErrNotFound
		}

		if closed.ParentUID != nil && *closed.ParentUID != "" {
			issue, err := lookup.IssueByUID(*closed.ParentUID)
			if err := record(issue, err); err != nil {
				return nil, err
			}
		} else if closed.ParentShortID != nil && *closed.ParentShortID != "" {
			if err := record(shortIssue(*closed.ParentShortID)); err != nil {
				return nil, err
			}
		}
		for _, ref := range closed.issueRefs() {
			if err := record(lookup.IssueByRef(event.ProjectUID, ref)); err != nil {
				return nil, err
			}
		}
	}
	for _, ref := range refs {
		if ref == "" {
			return nil, ErrNotFound
		}
		if err := record(lookup.IssueByUID(ref)); err != nil {
			return nil, err
		}
	}

	type replyRef struct {
		uid    string
		target *string
	}
	replies := []replyRef{{payload.CommentUID, payload.ReplyToUID}}
	for _, comment := range payload.Comments {
		replies = append(replies, replyRef{comment.CommentUID, comment.ReplyToUID})
	}
	for _, reply := range replies {
		if reply.target != nil && *reply.target != "" {
			if err := record(commentIssue(reply.uid, *reply.target)); err != nil {
				return nil, err
			}
		}
	}
	slot := func(raw jsontext.Value) error {
		var fields map[string]jsontext.Value
		if json.Unmarshal(raw, &fields) != nil || len(fields["re"]) == 0 {
			return nil
		}
		value, _, err := notification.DecodeValue(raw)
		if err != nil {
			return ErrNotFound
		}
		if value.Re == "" {
			return nil
		}
		return record(commentIssue("", value.Re))
	}
	for key, raw := range payload.Metadata {
		if strings.HasPrefix(event.Type, "issue.") && strings.HasPrefix(key, notification.KeyPrefix) {
			if err := slot(raw); err != nil {
				return nil, err
			}
		}
	}
	for key, change := range payload.Diff {
		if strings.HasPrefix(event.Type, "issue.") && strings.HasPrefix(key, notification.KeyPrefix) {
			for _, raw := range []jsontext.Value{change.From, change.To} {
				if err := slot(raw); err != nil {
					return nil, err
				}
			}
		}
	}
	return projects, nil
}

// RelayEventReferencedProjectsTx resolves native references against live rows
// and retained ownership facts, using the backend's fixed SQL conventions.
func RelayEventReferencedProjectsTx(ctx context.Context, tx *sql.Tx, event RemoteEvent, refs []string, live func(string) (Issue, error), bind func(int) string, field func(string) string) ([]int64, error) {
	projects, _, err := RelayEventReferencedProjectsWithPurgeFactsTx(ctx, tx, event, refs, live, bind, field, nil)
	return projects, err
}

// RelayEventReferencedProjectsWithPurgeFactsTx also reports whether resolving
// a reference required a retained purge fact. Fresh root checkpoints use this
// to omit historical rows whose ownership proof will not travel with them.
func RelayEventReferencedProjectsWithPurgeFactsTx(ctx context.Context, tx *sql.Tx, event RemoteEvent, refs []string, live func(string) (Issue, error), bind func(int) string, field func(string) string, purged func(string) (bool, error)) ([]int64, bool, error) {
	var usedPurgeFact bool
	owner := func(query string, args ...any) (Issue, error) {
		var issue Issue
		var owners, subjects int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT project_id),COUNT(DISTINCT issue_uid),COALESCE(MIN(project_id),0),COALESCE(MIN(issue_uid),'') FROM (`+query+`) candidates`, args...).Scan(&owners, &subjects, &issue.ProjectID, &issue.UID)
		if err != nil {
			return Issue{}, err
		}
		if owners != 1 || subjects != 1 {
			return Issue{}, ErrNotFound
		}
		current, err := live(issue.UID)
		if !errors.Is(err, ErrNotFound) {
			return current, err
		}
		if purged != nil {
			fromPurge, err := purged(issue.UID)
			if err != nil {
				return Issue{}, err
			}
			usedPurgeFact = usedPurgeFact || fromPurge
		}
		return issue, nil
	}
	byUID := func(uid string) (Issue, error) {
		issue, err := live(uid)
		if !errors.Is(err, ErrNotFound) {
			return issue, err
		}
		return owner(`SELECT project_id,issue_uid FROM events WHERE issue_uid=`+bind(1)+` AND type IN ('issue.created','issue.snapshot') UNION SELECT project_id,issue_uid FROM purge_log WHERE issue_uid=`+bind(2), uid, uid)
	}
	byShortID := func(projectUID, shortID string) (Issue, error) {
		rows, err := tx.QueryContext(ctx, `
SELECT e.issue_uid
FROM events e JOIN projects p ON p.id=e.project_id
WHERE e.issue_uid IS NOT NULL AND e.type IN ('issue.created','issue.snapshot')
  AND p.uid=`+bind(1)+` AND `+field("short_id")+`=`+bind(2)+`
UNION
SELECT e.issue_uid
FROM events e
WHERE e.issue_uid IS NOT NULL AND e.type='issue.moved'
  AND ((`+field("from_project_uid")+`=`+bind(3)+` AND `+field("from_short_id")+`=`+bind(4)+`)
    OR (`+field("to_project_uid")+`=`+bind(5)+` AND `+field("to_short_id")+`=`+bind(6)+`))`,
			projectUID, shortID, projectUID, shortID, projectUID, shortID) // #nosec G202 -- Query fragments and JSON fields are fixed; reference values use backend bind placeholders.
		if err != nil {
			return Issue{}, err
		}
		var candidates []string
		for rows.Next() {
			var uid string
			if err := rows.Scan(&uid); err != nil {
				_ = rows.Close()
				return Issue{}, err
			}
			candidates = append(candidates, uid)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return Issue{}, err
		}
		if err := rows.Close(); err != nil {
			return Issue{}, err
		}
		var matchedUID string
		for _, candidateUID := range candidates {
			matched, err := relayIssueMatchesShortIDAtEvent(ctx, tx, candidateUID, projectUID, shortID, event, bind)
			if err != nil {
				return Issue{}, err
			}
			if !matched {
				continue
			}
			if matchedUID != "" && matchedUID != candidateUID {
				return Issue{}, ErrNotFound
			}
			matchedUID = candidateUID
		}
		if matchedUID == "" {
			return Issue{}, ErrNotFound
		}
		return byUID(matchedUID)
	}
	projectByName := func(originInstanceUID, name string) (string, error) {
		var projectCount int
		var projectID int64
		err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT project_id),COALESCE(MIN(project_id),0) FROM (
SELECT project_id FROM events WHERE origin_instance_uid=`+bind(1)+` AND project_name=`+bind(2)+`
UNION SELECT project_id FROM purge_log WHERE origin_instance_uid=`+bind(3)+` AND project_name=`+bind(4)+`) named`,
			originInstanceUID, name, originInstanceUID, name).Scan(&projectCount, &projectID)
		if err != nil {
			return "", err
		}
		if projectCount != 1 {
			return "", ErrNotFound
		}
		var projectUID string
		if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=`+bind(1), projectID).Scan(&projectUID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrNotFound
			}
			return "", err
		}
		return projectUID, nil
	}
	byRef := func(project, ref string) (Issue, error) {
		return relayReferenceByRef(event, project, ref, byUID, byShortID, projectByName)
	}
	comment := func(uid, target string) (Issue, error) {
		issue, err := owner(`SELECT i.project_id,i.uid AS issue_uid FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=`+bind(1), target)
		if !errors.Is(err, ErrNotFound) {
			return issue, err
		}
		// A retained reply identifies its target issue, not that issue's owner
		// after a possible move. Purge records independently retain ownership.
		query := `SELECT p.project_id,p.issue_uid FROM events e JOIN purge_log p ON p.issue_uid=e.related_issue_uid WHERE e.type='issue.commented' AND e.related_issue_uid IS NOT NULL AND e.related_issue_uid<>'' AND ` + field("reply_to_uid") + `=` + bind(1)
		args := []any{target}
		if uid != "" && event.IssueUID != nil {
			query += ` AND e.issue_uid=` + bind(2) + ` AND ` + field("comment_uid") + `=` + bind(3)
			args = append(args, *event.IssueUID, uid)
		}
		return owner(query, args...)
	}
	short := func(ref string) (Issue, error) {
		return owner(`SELECT project_id,uid AS issue_uid FROM issues WHERE short_id=`+bind(1), ref)
	}
	projects, err := RelayEventReferencedProjects(event, refs, NewEventIssueLookup(byUID, byRef), comment, short)
	return projects, usedPurgeFact, err
}

// RelayCheckpointMetadata drops only notification comment references that the
// caller has proved are dangling and authorized to omit. Fresh checkpoints do
// not carry root-local purge facts, but the remaining notification content is
// still current state.
func RelayCheckpointMetadata(raw jsontext.Value, canOmitComment func(string) (bool, error)) (jsontext.Value, error) {
	var slots map[string]jsontext.Value
	if err := json.Unmarshal(raw, &slots); err != nil {
		return nil, err
	}
	changed := false
	for key, slot := range slots {
		if !strings.HasPrefix(key, notification.KeyPrefix) {
			continue
		}
		var fields map[string]jsontext.Value
		if json.Unmarshal(slot, &fields) != nil || len(fields["re"]) == 0 {
			continue
		}
		value, _, err := notification.DecodeValue(slot)
		if err != nil || value.Re == "" {
			continue
		}
		omit, err := canOmitComment(value.Re)
		if err != nil {
			return nil, err
		}
		if !omit {
			continue
		}
		delete(fields, "re")
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		slots[key] = jsontext.Value(encoded)
		changed = true
	}
	if !changed {
		return raw, nil
	}
	encoded, err := json.Marshal(slots)
	if err != nil {
		return nil, err
	}
	return jsontext.Value(encoded), nil
}

func relayReferenceByRef(event RemoteEvent, project, ref string, byUID func(string) (Issue, error), byShortID func(string, string) (Issue, error), projectByName func(string, string) (string, error)) (Issue, error) {
	parsed, err := shortid.Parse(ref)
	if err != nil {
		return Issue{}, ErrNotFound
	}
	if parsed.ULID != "" {
		return byUID(parsed.ULID)
	}
	// An own-name qualifier requires a concrete target in the canonical
	// source namespace; the unhashed display name is only a routing clue.
	if parsed.Project == "" || parsed.Project == event.ProjectName {
		return byShortID(project, parsed.ShortID)
	}
	// The producer's project name is retained on native history, independent
	// of local aliases. A name reused by multiple source projects is ambiguous.
	projectUID, err := projectByName(event.OriginInstanceUID, parsed.Project)
	if err != nil {
		return Issue{}, err
	}
	return byShortID(projectUID, parsed.ShortID)
}

func relayIssueMatchesShortIDAtEvent(ctx context.Context, tx *sql.Tx, issueUID, projectUID, shortID string, event RemoteEvent, bind func(int) string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.uid,e.type,e.origin_instance_uid,e.uid,e.hlc_physical_ms,e.hlc_counter,e.payload
FROM events e JOIN projects p ON p.id=e.project_id
WHERE e.issue_uid=`+bind(1)+` AND e.type IN ('issue.created','issue.snapshot','issue.moved')
ORDER BY e.hlc_physical_ms,e.hlc_counter,e.origin_instance_uid,e.uid`, issueUID) // #nosec G202 -- The query uses a backend bind placeholder for the issue UID.
	if err != nil {
		return false, err
	}
	referenceClock := FoldClock{HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, OriginInstanceUID: event.OriginInstanceUID, EventUID: event.EventUID}
	var history []FoldEvent
	hasIdentity := false
	for rows.Next() {
		var project, eventType, originInstanceUID, eventUID string
		var physicalMS, counter int64
		var payload []byte
		if err := rows.Scan(&project, &eventType, &originInstanceUID, &eventUID, &physicalMS, &counter, &payload); err != nil {
			_ = rows.Close()
			return false, err
		}
		foldEvent := FoldEvent{
			UID: eventUID, OriginInstanceUID: originInstanceUID, ProjectUID: project,
			IssueUID: issueUID, Type: eventType, HLCPhysicalMS: physicalMS,
			HLCCounter: counter, Payload: jsontext.Value(payload),
		}
		if compareClock(clockOf(foldEvent), referenceClock) > 0 {
			continue
		}
		if eventType == "issue.created" || eventType == "issue.snapshot" {
			hasIdentity = true
		}
		history = append(history, foldEvent)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if !hasIdentity {
		return false, nil
	}
	issue, ok := FoldEvents(history).Issues[issueUID]
	return ok && issue.ProjectUID == projectUID && issue.ShortID == shortID, nil
}
