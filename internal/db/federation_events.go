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

	"go.kenn.io/kata/internal/teammate"
)

// EventTimestampFormat is the millisecond UTC representation covered by portable event hashes.
const EventTimestampFormat = "2006-01-02T15:04:05.000Z"

type federationLocalReplayDeliveryKey struct{}

// WithFederationLocalReplayDelivery records delivery debt for new local-origin
// replays inserted for a pull page that has not advanced its cursor.
func WithFederationLocalReplayDelivery(ctx context.Context, due bool) context.Context {
	return context.WithValue(ctx, federationLocalReplayDeliveryKey{}, due)
}

// FederationLocalReplayDeliveryDue reports whether the current pull insert
// must commit replay delivery debt along with the event.
func FederationLocalReplayDeliveryDue(ctx context.Context) bool {
	due, _ := ctx.Value(federationLocalReplayDeliveryKey{}).(bool)
	return due
}

// ReadFederationLocalReplayStatus distinguishes a quiet replay from an older
// event that has no replay marker.
func ReadFederationLocalReplayStatus(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, eventUID string) (due, recorded bool, err error) {
	var value int
	err = q.QueryRowContext(ctx, `SELECT delivery_due FROM federation_local_replays WHERE event_uid=$1`, eventUID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return value == 1, err == nil, err
}

// RecordFederationLocalReplayDue preserves proven legacy delivery debt before
// projection. A recorded quiet replay stays quiet.
func RecordFederationLocalReplayDue(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, eventUID string) error {
	_, err := execer.ExecContext(ctx, `
		INSERT INTO federation_local_replays(event_uid, delivery_due) VALUES($1, 1)
		ON CONFLICT(event_uid) DO NOTHING`, eventUID)
	return err
}

// ClaimFederationLocalReplays closes delivery debt in the pull cursor
// transaction. The caller has already verified missing projection for any
// candidate that lacks a replay marker.
func ClaimFederationLocalReplays(ctx context.Context, tx *sql.Tx, eventUIDs []string) ([]string, error) {
	ordered := slices.Clone(eventUIDs)
	slices.Sort(ordered)
	claimed := make([]string, 0, len(ordered))
	for chunk := range slices.Chunk(ordered, 500) {
		args := make([]any, len(chunk))
		placeholders := make([]string, len(chunk))
		for i, uid := range chunk {
			args[i] = uid
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		//nolint:gosec // The query only joins generated placeholders.
		rows, err := tx.QueryContext(ctx, `
			INSERT INTO federation_local_replays(event_uid, delivery_due)
			SELECT uid, 0 FROM events WHERE uid IN (`+strings.Join(placeholders, ",")+`) ORDER BY uid
			ON CONFLICT(event_uid) DO UPDATE SET delivery_due=0
			WHERE federation_local_replays.delivery_due=1
			RETURNING event_uid`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var uid string
			if err := rows.Scan(&uid); err != nil {
				_ = rows.Close()
				return nil, err
			}
			claimed = append(claimed, uid)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return claimed, nil
}

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
	if eventType != "issue.snapshot" && eventType != "issue.created" {
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
