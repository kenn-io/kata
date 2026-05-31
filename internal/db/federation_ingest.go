package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrFederationIngestValidation reports a remote push event that is well-formed
// JSON but invalid for the requested hub project/scope.
var ErrFederationIngestValidation = errors.New("federation ingest validation")

// FederationIngestEvent carries one spoke event plus the spoke-local event row
// id used only for push cursor acknowledgement.
type FederationIngestEvent struct {
	SourceEventID int64
	Event         RemoteEvent
}

// FederationIngestParams is the all-or-nothing DB ingest boundary used by the
// hub transport handler.
type FederationIngestParams struct {
	ProjectID        int64
	SpokeInstanceUID string
	Events           []FederationIngestEvent
}

// FederationIngestResult summarizes an accepted batch. InsertedEventUIDs lists
// only fresh events, including generated claim audit events in insertion order,
// so callers can avoid rebroadcasting response-lost retries.
type FederationIngestResult struct {
	Accepted          int
	Duplicates        int
	PushCursorEventID int64
	InsertedEventUIDs []string
}

type preparedFederationIngestEvent struct {
	SourceEventID int64
	Event         RemoteEvent
	Duplicate     bool
}

// IngestFederationEvents validates and stores a spoke push batch. The batch is
// all-or-nothing: any invalid event, conflicting duplicate, or materialization
// failure rolls back every insert from the batch.
func (d *DB) IngestFederationEvents(
	ctx context.Context,
	p FederationIngestParams,
) (FederationIngestResult, error) {
	var result FederationIngestResult
	err := RetryLockContention(ctx, func() error {
		var err error
		result, err = d.ingestFederationEventsOnce(ctx, p)
		return err
	})
	return result, err
}

func (d *DB) ingestFederationEventsOnce(
	ctx context.Context,
	p FederationIngestParams,
) (FederationIngestResult, error) {
	if len(p.Events) == 0 {
		return FederationIngestResult{}, nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return FederationIngestResult{}, fmt.Errorf("begin federation ingest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	projectUID, projectName, err := requireFederationIngestHub(ctx, tx, p.ProjectID)
	if err != nil {
		return FederationIngestResult{}, err
	}
	knownIssueUIDs, err := currentFederatedIssueUIDSet(ctx, tx, p.ProjectID)
	if err != nil {
		return FederationIngestResult{}, err
	}
	batchCreateSnapshotUIDs, err := federationIngestCreateSnapshotUIDSet(p.Events)
	if err != nil {
		return FederationIngestResult{}, err
	}
	prepared := make([]preparedFederationIngestEvent, 0, len(p.Events))
	result := FederationIngestResult{}
	seenBatch := map[string]string{}
	freshSnapshotSeen := false
	for _, in := range p.Events {
		if in.SourceEventID <= 0 {
			return FederationIngestResult{}, fmt.Errorf("%w: source event id must be positive", ErrFederationIngestValidation)
		}
		if in.SourceEventID > result.PushCursorEventID {
			result.PushCursorEventID = in.SourceEventID
		}
		ev := in.Event
		if len(ev.Payload) == 0 {
			ev.Payload = json.RawMessage(`{}`)
		}
		if err := validateFederationProjectEvent(projectUID, p.SpokeInstanceUID, ev, knownIssueUIDs, batchCreateSnapshotUIDs); err != nil {
			return FederationIngestResult{}, err
		}
		if err := validateFederationEventHash(ev); err != nil {
			return FederationIngestResult{}, err
		}
		if existingHash, ok := seenBatch[ev.EventUID]; ok {
			if existingHash != ev.ContentHash {
				return FederationIngestResult{}, fmt.Errorf("%w: event %s", ErrRemoteEventConflict, ev.EventUID)
			}
			result.Duplicates++
			prepared = append(prepared, preparedFederationIngestEvent{
				SourceEventID: in.SourceEventID,
				Event:         ev,
				Duplicate:     true,
			})
			continue
		}
		existingHash, err := federationEventHashByUID(ctx, tx, ev.EventUID)
		if err == nil {
			if existingHash != ev.ContentHash {
				return FederationIngestResult{}, fmt.Errorf("%w: event %s", ErrRemoteEventConflict, ev.EventUID)
			}
			result.Duplicates++
			rememberIngestIssueUIDs(ev, knownIssueUIDs)
			prepared = append(prepared, preparedFederationIngestEvent{
				SourceEventID: in.SourceEventID,
				Event:         ev,
				Duplicate:     true,
			})
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return FederationIngestResult{}, err
		}
		if freshSnapshotSeen && ev.Type != "issue.snapshot" {
			return FederationIngestResult{}, fmt.Errorf("%w: non-snapshot event %s follows snapshot baseline in same batch",
				ErrFederationIngestValidation, ev.EventUID)
		}
		if err := rejectFreshCreateSnapshotForKnownIssue(ev, knownIssueUIDs); err != nil {
			return FederationIngestResult{}, err
		}
		if ev.Type == "issue.snapshot" {
			freshSnapshotSeen = true
		}
		seenBatch[ev.EventUID] = ev.ContentHash
		rememberIngestIssueUIDs(ev, knownIssueUIDs)
		prepared = append(prepared, preparedFederationIngestEvent{
			SourceEventID: in.SourceEventID,
			Event:         ev,
		})
	}

	for _, in := range prepared {
		if in.Duplicate {
			continue
		}
		ev := in.Event
		inserted, err := insertFederationEventTx(ctx, tx, p.ProjectID, projectName, ev)
		if err != nil {
			return FederationIngestResult{}, err
		}
		if !inserted {
			result.Duplicates++
			continue
		}
		// claim.violated is best-effort audit metadata evaluated against
		// current hub claim state at ingest time. It is not a causally precise
		// historical authorization judgment for offline work.
		auditEvents, err := d.annotateFederationIngestClaimWorkTx(ctx, tx, p.ProjectID, projectName, ev)
		if err != nil {
			return FederationIngestResult{}, err
		}
		result.Accepted++
		result.InsertedEventUIDs = append(result.InsertedEventUIDs, ev.EventUID)
		for _, auditEvent := range auditEvents {
			result.InsertedEventUIDs = append(result.InsertedEventUIDs, auditEvent.UID)
		}
	}
	if result.Accepted > 0 {
		if err := d.materializeFederatedProjectTx(ctx, tx, p.ProjectID); err != nil {
			return FederationIngestResult{}, err
		}
	}
	if err := federationFailpoint("before_federation_ingest_commit"); err != nil {
		return FederationIngestResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return FederationIngestResult{}, fmt.Errorf("commit federation ingest: %w", err)
	}
	return result, nil
}

func insertFederationEventTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	projectName string,
	ev RemoteEvent,
) (bool, error) {
	storedProjectName := ev.ProjectName
	if storedProjectName == "" {
		storedProjectName = projectName
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO events(
		   uid, origin_instance_uid, project_id, project_name,
		   issue_id, issue_uid, related_issue_id, related_issue_uid,
		   type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at
		 )
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(uid) DO NOTHING`,
		ev.EventUID, ev.OriginInstanceUID,
		projectID, storedProjectName,
		nil, stringPtrValue(ev.IssueUID),
		nil, stringPtrValue(ev.RelatedIssueUID),
		ev.Type, ev.Actor, string(ev.Payload),
		ev.HLCPhysicalMS, ev.HLCCounter, ev.ContentHash,
		ev.CreatedAt.UTC().Format(sqliteTimeFormat))
	if err != nil {
		return false, fmt.Errorf("insert federation event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert federation event rows affected: %w", err)
	}
	if affected > 0 {
		return true, nil
	}
	existingHash, err := federationEventHashByUID(ctx, tx, ev.EventUID)
	if err != nil {
		return false, err
	}
	if existingHash != ev.ContentHash {
		return false, fmt.Errorf("%w: event %s", ErrRemoteEventConflict, ev.EventUID)
	}
	return false, nil
}

func requireFederationIngestHub(ctx context.Context, tx *sql.Tx, projectID int64) (string, string, error) {
	var projectUID, projectName, role string
	var enabled int
	err := tx.QueryRowContext(ctx, `
		SELECT p.uid, p.name, fb.role, fb.enabled
		  FROM projects p
		  JOIN federation_bindings fb ON fb.project_id = p.id
		 WHERE p.id = ? AND p.deleted_at IS NULL`, projectID).
		Scan(&projectUID, &projectName, &role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("lookup federation ingest hub: %w", err)
	}
	if role != string(FederationRoleHub) || enabled != 1 {
		return "", "", fmt.Errorf("%w: project is not an enabled federation hub", ErrFederationIngestValidation)
	}
	return projectUID, projectName, nil
}

func validateFederationEventHash(ev RemoteEvent) error {
	expectedHash, err := EventContentHash(EventHashInput{
		UID:               ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		RelatedIssueUID:   ev.RelatedIssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		HLCPhysicalMS:     ev.HLCPhysicalMS,
		HLCCounter:        ev.HLCCounter,
		CreatedAt:         ev.CreatedAt.UTC().Format(sqliteTimeFormat),
		Payload:           ev.Payload,
	})
	if err != nil {
		return fmt.Errorf("federation event content hash: %w", err)
	}
	if !strings.EqualFold(expectedHash, ev.ContentHash) {
		return fmt.Errorf("%w: event %s", ErrRemoteEventHashMismatch, ev.EventUID)
	}
	return nil
}

func federationEventHashByUID(ctx context.Context, tx *sql.Tx, eventUID string) (string, error) {
	var hash string
	err := tx.QueryRowContext(ctx,
		`SELECT content_hash FROM events WHERE uid = ?`, eventUID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup federation event duplicate: %w", err)
	}
	return hash, nil
}

func validateFederationProjectEvent(
	projectUID, spokeInstanceUID string,
	ev RemoteEvent,
	knownIssueUIDs map[string]struct{},
	batchCreateSnapshotUIDs map[string]struct{},
) error {
	if ev.ProjectUID != projectUID {
		return fmt.Errorf("%w: event %s targets project %s", ErrFederationIngestValidation, ev.EventUID, ev.ProjectUID)
	}
	if ev.OriginInstanceUID != spokeInstanceUID {
		return fmt.Errorf("%w: event %s origin mismatch", ErrFederationIngestValidation, ev.EventUID)
	}
	if ev.EventUID == "" || ev.HLCPhysicalMS <= 0 || ev.HLCCounter < 0 || strings.TrimSpace(ev.Actor) == "" {
		return fmt.Errorf("%w: event %s has invalid envelope", ErrFederationIngestValidation, ev.EventUID)
	}
	if strings.HasPrefix(ev.Type, "recurrence.") || ev.Type == "issue.moved" {
		return fmt.Errorf("%w: event type %s unsupported in phase 2", ErrFederationIngestValidation, ev.Type)
	}
	payload := payloadMap(ev.Payload)
	if ev.Type == "project.metadata_updated" {
		if payloadProjectUID, ok := stringValue(payload["project_uid"]); ok && payloadProjectUID != projectUID {
			return fmt.Errorf("%w: project metadata payload targets %s", ErrFederationIngestValidation, payloadProjectUID)
		}
		return nil
	}
	issueUID, err := payloadIssueUID(ev, payload)
	if err != nil {
		return err
	}
	switch ev.Type {
	case "issue.created", "issue.snapshot":
		if issueUID == "" {
			return fmt.Errorf("%w: %s missing issue uid", ErrFederationIngestValidation, ev.Type)
		}
	case "issue.updated", "issue.assigned", "issue.unassigned",
		"issue.priority_set", "issue.priority_cleared",
		"issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored",
		"issue.commented", "issue.labeled", "issue.unlabeled",
		"issue.linked", "issue.unlinked", "issue.links_changed", "issue.metadata_updated":
		if issueUID == "" {
			return fmt.Errorf("%w: %s missing issue uid", ErrFederationIngestValidation, ev.Type)
		}
		if _, ok := knownIssueUIDs[issueUID]; !ok {
			return fmt.Errorf("%w: %s references unknown issue %s", ErrFederationIngestValidation, ev.Type, issueUID)
		}
	default:
		return fmt.Errorf("%w: unsupported event type %s", ErrFederationIngestValidation, ev.Type)
	}
	deferredSnapshotLinks := map[string]struct{}{}
	if ev.Type == "issue.snapshot" {
		for _, ref := range payloadLinkIssueUIDs(ev) {
			if _, ok := batchCreateSnapshotUIDs[ref]; ok {
				deferredSnapshotLinks[ref] = struct{}{}
			}
		}
	}
	for _, ref := range payloadReferencedIssueUIDs(ev, payload) {
		if ref == issueUID {
			continue
		}
		if _, ok := knownIssueUIDs[ref]; !ok {
			if _, deferred := deferredSnapshotLinks[ref]; deferred {
				continue
			}
			return fmt.Errorf("%w: event %s references unknown issue %s", ErrFederationIngestValidation, ev.EventUID, ref)
		}
	}
	return nil
}

func federationIngestCreateSnapshotUIDSet(events []FederationIngestEvent) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, in := range events {
		ev := in.Event
		if len(ev.Payload) == 0 {
			ev.Payload = json.RawMessage(`{}`)
		}
		switch ev.Type {
		case "issue.created", "issue.snapshot":
		default:
			continue
		}
		uid, err := payloadIssueUID(ev, payloadMap(ev.Payload))
		if err != nil {
			return nil, err
		}
		if uid != "" {
			out[uid] = struct{}{}
		}
	}
	return out, nil
}

func rejectFreshCreateSnapshotForKnownIssue(ev RemoteEvent, knownIssueUIDs map[string]struct{}) error {
	switch ev.Type {
	case "issue.created", "issue.snapshot":
	default:
		return nil
	}
	issueUID, err := payloadIssueUID(ev, payloadMap(ev.Payload))
	if err != nil {
		return err
	}
	if _, ok := knownIssueUIDs[issueUID]; ok {
		return fmt.Errorf("%w: fresh %s targets existing issue %s",
			ErrFederationIngestValidation, ev.Type, issueUID)
	}
	return nil
}

func payloadIssueUID(ev RemoteEvent, payload map[string]json.RawMessage) (string, error) {
	var payloadUID string
	if uid, ok := stringValue(payload["issue_uid"]); ok {
		payloadUID = uid
	}
	if uid, ok := stringValue(payload["uid"]); ok {
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

func payloadReferencedIssueUIDs(ev RemoteEvent, payload map[string]json.RawMessage) []string {
	var refs []string
	if ev.RelatedIssueUID != nil && *ev.RelatedIssueUID != "" {
		refs = append(refs, *ev.RelatedIssueUID)
	}
	for _, key := range []string{
		"from_uid", "to_uid", "from_issue_uid", "to_issue_uid",
		"parent_set_uid", "parent_removed_uid",
	} {
		if uid, ok := stringValue(payload[key]); ok {
			refs = append(refs, uid)
		}
	}
	for _, key := range []string{
		"blocks_added_uids", "blocks_removed_uids",
		"blocked_by_added_uids", "blocked_by_removed_uids",
		"related_added_uids", "related_removed_uids",
	} {
		refs = append(refs, stringSlice(payload[key])...)
	}
	refs = append(refs, payloadLinkIssueUIDs(ev)...)
	return refs
}

func payloadLinkIssueUIDs(ev RemoteEvent) []string {
	var created struct {
		Links []struct {
			ToIssueUID string `json:"to_issue_uid"`
		} `json:"links"`
	}
	_ = json.Unmarshal(ev.Payload, &created)
	var refs []string
	for _, link := range created.Links {
		if link.ToIssueUID != "" {
			refs = append(refs, link.ToIssueUID)
		}
	}
	return refs
}

func currentFederatedIssueUIDSet(ctx context.Context, tx *sql.Tx, projectID int64) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	rows, err := tx.QueryContext(ctx, `SELECT uid FROM issues WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list current issue uids: %w", err)
	}
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan current issue uid: %w", err)
		}
		out[uid] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close current issue uids: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current issue uids: %w", err)
	}
	eventRows, err := tx.QueryContext(ctx, `
		SELECT issue_uid FROM events WHERE project_id = ? AND issue_uid IS NOT NULL
		UNION
		SELECT related_issue_uid FROM events WHERE project_id = ? AND related_issue_uid IS NOT NULL`,
		projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("list event issue uids: %w", err)
	}
	defer func() { _ = eventRows.Close() }()
	for eventRows.Next() {
		var uid string
		if err := eventRows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("scan event issue uid: %w", err)
		}
		out[uid] = struct{}{}
	}
	return out, eventRows.Err()
}

func rememberIngestIssueUIDs(ev RemoteEvent, known map[string]struct{}) {
	payload := payloadMap(ev.Payload)
	if uid, err := payloadIssueUID(ev, payload); err == nil && uid != "" {
		switch ev.Type {
		case "issue.created", "issue.snapshot":
			known[uid] = struct{}{}
		}
	}
}
