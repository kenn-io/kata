package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/internal/shortid"
	katauid "go.kenn.io/kata/internal/uid"
)

// ErrRemoteEventHashMismatch reports a remote event whose advertised content
// hash does not match the portable event fields.
var ErrRemoteEventHashMismatch = errors.New("remote event content hash mismatch")

// ErrRemoteEventConflict reports a duplicate remote event UID with different
// content than the row already stored locally.
var ErrRemoteEventConflict = errors.New("remote event uid conflict")

// ErrFederatedReadOnly is returned when a local mutation targets an enabled
// spoke replica whose local writes are not available for that operation.
var ErrFederatedReadOnly = errors.New("federated spoke project is read-only")

// ErrFederatedSpokeUnsupported is joined with ErrFederatedReadOnly for Phase 2
// operations that remain hub-only even when a spoke is push-enabled.
var ErrFederatedSpokeUnsupported = errors.New("federated spoke operation unsupported")

// ErrFederatedMoveUnsupported is returned when a move involves any federated
// project. Cross-project federated move semantics need a separate design.
var ErrFederatedMoveUnsupported = errors.New("federated project move unsupported")

// ListFederationBindings returns every configured federation binding ordered by
// local project id. A fresh non-federated database returns an empty non-nil
// slice.
func (d *DB) ListFederationBindings(ctx context.Context) ([]FederationBinding, error) {
	rows, err := d.QueryContext(ctx, federationBindingSelect+` ORDER BY project_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list federation bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []FederationBinding{}
	for rows.Next() {
		b, err := scanFederationBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// FederationBindingByProject returns the binding for one local project.
func (d *DB) FederationBindingByProject(ctx context.Context, projectID int64) (FederationBinding, error) {
	return scanFederationBinding(d.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id = ?`, projectID))
}

// FederationSyncStatus mirrors federation_sync_status for operator-facing
// federation health and last-attempt state.
type FederationSyncStatus struct {
	ProjectID         int64
	LastPullStartedAt *time.Time
	LastPullSuccessAt *time.Time
	LastPushStartedAt *time.Time
	LastPushSuccessAt *time.Time
	LastErrorAt       *time.Time
	LastError         *string
	LastResetAt       *time.Time
}

// RecordFederationQuarantineParams records a poisoned federation batch.
type RecordFederationQuarantineParams struct {
	ProjectID    int64
	Direction    FederationQuarantineDirection
	FirstEventID int64
	LastEventID  int64
	EventUIDs    []string
	Error        string
	CreatedAt    time.Time
}

// SkipFederationQuarantineParams resolves an active quarantine by advancing the
// relevant cursor past the quarantined batch.
type SkipFederationQuarantineParams struct {
	ID        int64
	ProjectID int64
	Actor     string
	Reason    string
	Now       time.Time
}

// AdoptProjectIntoFederationParams configures adoption of an existing local
// project into a hub federation.
type AdoptProjectIntoFederationParams struct {
	ProjectID            int64
	HubURL               string
	HubProjectID         int64
	HubProjectUID        string
	ReplayHorizonEventID int64
	Actor                string
}

// AdoptProjectIntoFederationResult describes the adopted project, binding, and
// snapshot emission count.
type AdoptProjectIntoFederationResult struct {
	Project               Project
	Binding               FederationBinding
	AdoptionSnapshotCount int64
}

// FederationSyncStatusByProject returns the stored sync status for one local
// federation project.
func (d *DB) FederationSyncStatusByProject(ctx context.Context, projectID int64) (FederationSyncStatus, error) {
	return scanFederationSyncStatus(d.QueryRowContext(ctx, `
		SELECT project_id, last_pull_started_at, last_pull_success_at,
		       last_push_started_at, last_push_success_at,
		       last_error_at, last_error, last_reset_at
		  FROM federation_sync_status
		 WHERE project_id = ?`, projectID))
}

// RecordFederationSyncPullStarted records that a pull attempt began.
func (d *DB) RecordFederationSyncPullStarted(ctx context.Context, projectID int64, at time.Time) error {
	return d.upsertFederationSyncTime(ctx, projectID, "last_pull_started_at", at)
}

// RecordFederationSyncPullSuccess records that a pull attempt completed.
func (d *DB) RecordFederationSyncPullSuccess(ctx context.Context, projectID int64, at time.Time) error {
	return d.upsertFederationSyncTime(ctx, projectID, "last_pull_success_at", at)
}

// RecordFederationSyncPushStarted records that outbound federation work began.
func (d *DB) RecordFederationSyncPushStarted(ctx context.Context, projectID int64, at time.Time) error {
	return d.upsertFederationSyncTime(ctx, projectID, "last_push_started_at", at)
}

// RecordFederationSyncPushSuccess records that outbound federation work completed.
func (d *DB) RecordFederationSyncPushSuccess(ctx context.Context, projectID int64, at time.Time) error {
	return d.upsertFederationSyncTime(ctx, projectID, "last_push_success_at", at)
}

// RecordFederationSyncReset records that a reset completed.
func (d *DB) RecordFederationSyncReset(ctx context.Context, projectID int64, at time.Time) error {
	return d.upsertFederationSyncTime(ctx, projectID, "last_reset_at", at)
}

// RecordFederationSyncError records the latest federation sync error.
func (d *DB) RecordFederationSyncError(ctx context.Context, projectID int64, syncErr error, at time.Time) error {
	msg := ""
	if syncErr != nil {
		msg = syncErr.Error()
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO federation_sync_status(project_id, last_error_at, last_error)
		VALUES(?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			last_error_at = excluded.last_error_at,
			last_error = excluded.last_error`,
		projectID, at.UTC().Format(sqliteTimeFormat), msg)
	if err != nil {
		return fmt.Errorf("record federation sync error: %w", err)
	}
	return nil
}

// ClearFederationSyncError clears the project-level sync error after the whole
// per-binding runner pass succeeds.
func (d *DB) ClearFederationSyncError(ctx context.Context, projectID int64) error {
	_, err := d.ExecContext(ctx, `
		INSERT INTO federation_sync_status(project_id, last_error_at, last_error)
		VALUES(?, NULL, NULL)
		ON CONFLICT(project_id) DO UPDATE SET
			last_error_at = NULL,
			last_error = NULL`, projectID)
	if err != nil {
		return fmt.Errorf("clear federation sync error: %w", err)
	}
	return nil
}

// RecordFederationQuarantine creates or returns the active quarantine for one
// project/direction. A second failure while a quarantine is active preserves
// the original poisoned batch so status and skip stay stable.
func (d *DB) RecordFederationQuarantine(
	ctx context.Context,
	p RecordFederationQuarantineParams,
) (FederationQuarantine, error) {
	if err := validateFederationQuarantine(p); err != nil {
		return FederationQuarantine{}, err
	}
	eventUIDs, err := json.Marshal(p.EventUIDs)
	if err != nil {
		return FederationQuarantine{}, fmt.Errorf("encode federation quarantine event uids: %w", err)
	}
	_, err = d.ExecContext(ctx, `
		INSERT INTO federation_quarantine(
			project_id, direction, first_event_id, last_event_id, event_uids, error, created_at
		)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, direction) WHERE skipped_at IS NULL DO NOTHING`,
		p.ProjectID, string(p.Direction), p.FirstEventID, p.LastEventID, string(eventUIDs),
		p.Error, p.CreatedAt.UTC().Format(sqliteTimeFormat))
	if err != nil {
		return FederationQuarantine{}, fmt.Errorf("record federation quarantine: %w", err)
	}
	return d.ActiveFederationQuarantine(ctx, p.ProjectID, p.Direction)
}

// ActiveFederationQuarantine returns the unresolved quarantine for one
// project/direction.
func (d *DB) ActiveFederationQuarantine(
	ctx context.Context,
	projectID int64,
	direction FederationQuarantineDirection,
) (FederationQuarantine, error) {
	return scanFederationQuarantine(d.QueryRowContext(ctx,
		federationQuarantineSelect+` WHERE project_id = ? AND direction = ? AND skipped_at IS NULL`,
		projectID, string(direction)))
}

// ActiveFederationQuarantinesByProject returns all unresolved quarantines for
// one project.
func (d *DB) ActiveFederationQuarantinesByProject(ctx context.Context, projectID int64) ([]FederationQuarantine, error) {
	rows, err := d.QueryContext(ctx,
		federationQuarantineSelect+` WHERE project_id = ? AND skipped_at IS NULL ORDER BY id ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list active federation quarantines: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanFederationQuarantines(rows)
}

// SkipFederationQuarantine marks a quarantine skipped and advances the matching
// cursor. Push quarantine skip never deletes local events; it only records that
// the operator intentionally moved the outbound cursor past them.
func (d *DB) SkipFederationQuarantine(ctx context.Context, p SkipFederationQuarantineParams) (FederationQuarantine, error) {
	actor := strings.TrimSpace(p.Actor)
	if actor == "" {
		return FederationQuarantine{}, fmt.Errorf("skip federation quarantine: actor is required")
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return FederationQuarantine{}, fmt.Errorf("begin skip federation quarantine: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q, err := scanFederationQuarantine(tx.QueryRowContext(ctx,
		federationQuarantineSelect+` WHERE id = ? AND project_id = ? AND skipped_at IS NULL`,
		p.ID, p.ProjectID))
	if err != nil {
		return FederationQuarantine{}, err
	}
	switch q.Direction {
	case FederationQuarantineDirectionPush:
		if _, err := tx.ExecContext(ctx, `
			UPDATE federation_bindings
			   SET push_cursor_event_id = CASE
			         WHEN push_cursor_event_id < ? THEN ?
			         ELSE push_cursor_event_id
			       END,
			       last_sync_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
			       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE project_id = ?`,
			q.LastEventID, q.LastEventID, q.ProjectID); err != nil {
			return FederationQuarantine{}, fmt.Errorf("advance quarantine push cursor: %w", err)
		}
	default:
		return FederationQuarantine{}, fmt.Errorf("skip federation quarantine: unsupported direction %q", q.Direction)
	}
	reason := strings.TrimSpace(p.Reason)
	if _, err := tx.ExecContext(ctx, `
		UPDATE federation_quarantine
		   SET skipped_at = ?, skipped_by = ?, skip_reason = ?
		 WHERE id = ?
		   AND project_id = ?
		   AND skipped_at IS NULL`,
		now.UTC().Format(sqliteTimeFormat), actor, reason, p.ID, p.ProjectID); err != nil {
		return FederationQuarantine{}, fmt.Errorf("mark federation quarantine skipped: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return FederationQuarantine{}, fmt.Errorf("commit skip federation quarantine: %w", err)
	}
	return scanFederationQuarantine(d.QueryRowContext(ctx,
		federationQuarantineSelect+` WHERE id = ? AND project_id = ?`, p.ID, p.ProjectID))
}

func (d *DB) upsertFederationSyncTime(ctx context.Context, projectID int64, column string, at time.Time) error {
	switch column {
	case "last_pull_started_at", "last_pull_success_at",
		"last_push_started_at", "last_push_success_at", "last_reset_at":
	default:
		return fmt.Errorf("unsupported federation sync status column %q", column)
	}
	_, err := d.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO federation_sync_status(project_id, %s)
		VALUES(?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			%s = excluded.%s`, column, column, column),
		projectID, at.UTC().Format(sqliteTimeFormat))
	if err != nil {
		return fmt.Errorf("record federation sync %s: %w", column, err)
	}
	return nil
}

// UpsertFederationBinding inserts or replaces one local federation binding.
func (d *DB) UpsertFederationBinding(ctx context.Context, b FederationBinding) (FederationBinding, error) {
	enabled := 0
	if b.Enabled {
		enabled = 1
	}
	pushEnabled := 0
	if b.PushEnabled {
		pushEnabled = 1
	}
	_, err := d.ExecContext(ctx, `
		INSERT INTO federation_bindings(
			project_id, role, hub_url, hub_project_id, hub_project_uid,
			replay_horizon_event_id, pull_cursor_event_id, push_enabled,
			push_cursor_event_id, enabled
		)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			role = excluded.role,
			hub_url = excluded.hub_url,
			hub_project_id = excluded.hub_project_id,
			hub_project_uid = excluded.hub_project_uid,
			replay_horizon_event_id = excluded.replay_horizon_event_id,
			pull_cursor_event_id = excluded.pull_cursor_event_id,
			push_enabled = excluded.push_enabled,
			push_cursor_event_id = excluded.push_cursor_event_id,
			enabled = excluded.enabled,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		b.ProjectID, string(b.Role), b.HubURL, b.HubProjectID, b.HubProjectUID,
		b.ReplayHorizonEventID, b.PullCursorEventID, pushEnabled, b.PushCursorEventID,
		enabled)
	if err != nil {
		return FederationBinding{}, fmt.Errorf("upsert federation binding: %w", err)
	}
	return d.FederationBindingByProject(ctx, b.ProjectID)
}

// AdvanceFederationPullCursor records the highest hub events.id consumed by a
// spoke binding.
func (d *DB) AdvanceFederationPullCursor(ctx context.Context, projectID, nextCursor int64) error {
	res, err := d.ExecContext(ctx, `
		UPDATE federation_bindings
		   SET pull_cursor_event_id = ?,
		       last_sync_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE project_id = ?`,
		nextCursor, projectID)
	if err != nil {
		return fmt.Errorf("advance federation pull cursor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance federation pull cursor rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// InsertRemoteEvent appends a hub event to the local log while preserving every
// portable field. Only the local events.id is assigned by the spoke database.
func (d *DB) InsertRemoteEvent(ctx context.Context, projectID int64, ev RemoteEvent) (bool, error) {
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	createdAt := ev.CreatedAt.UTC().Format(sqliteTimeFormat)
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
		CreatedAt:         createdAt,
		Payload:           payload,
	})
	if err != nil {
		return false, fmt.Errorf("remote event content hash: %w", err)
	}
	if !strings.EqualFold(expectedHash, ev.ContentHash) {
		return false, fmt.Errorf("%w: event %s", ErrRemoteEventHashMismatch, ev.EventUID)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin remote event insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingHash string
	err = tx.QueryRowContext(ctx,
		`SELECT content_hash FROM events WHERE uid = ?`, ev.EventUID).Scan(&existingHash)
	if err == nil {
		if existingHash == ev.ContentHash {
			if err := tx.Commit(); err != nil {
				return false, fmt.Errorf("commit duplicate remote event no-op: %w", err)
			}
			return false, nil
		}
		return false, fmt.Errorf("%w: event %s", ErrRemoteEventConflict, ev.EventUID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check remote event duplicate: %w", err)
	}

	clock := eventHLCTimestamp{PhysicalMS: ev.HLCPhysicalMS, Counter: ev.HLCCounter}
	if _, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:         projectID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		RelatedIssueUID:   ev.RelatedIssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		Payload:           string(payload),
		UID:               ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		HLC:               &clock,
		CreatedAt:         createdAt,
		ContentHash:       ev.ContentHash,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit remote event insert: %w", err)
	}
	return true, nil
}

// AdoptProjectIntoFederation converts an existing local project into a
// push-enabled spoke replica for the hub project UID. The adoption emits a
// current-state push baseline above the captured local push cursor floor.
func (d *DB) AdoptProjectIntoFederation(
	ctx context.Context,
	p AdoptProjectIntoFederationParams,
) (AdoptProjectIntoFederationResult, error) {
	actor := strings.TrimSpace(p.Actor)
	if actor == "" {
		actor = "federation"
	}
	if !katauid.Valid(p.HubProjectUID) {
		return AdoptProjectIntoFederationResult{}, fmt.Errorf("invalid hub project uid %q", p.HubProjectUID)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return AdoptProjectIntoFederationResult{}, fmt.Errorf("begin federation adoption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	project, err := scanProject(tx.QueryRowContext(ctx,
		projectSelect+` WHERE id = ?`, p.ProjectID))
	if err != nil {
		return AdoptProjectIntoFederationResult{}, err
	}
	if project.DeletedAt != nil {
		return AdoptProjectIntoFederationResult{}, fmt.Errorf("adopt project into federation: project %d is archived", p.ProjectID)
	}

	existing, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id = ?`, p.ProjectID))
	if err == nil {
		if existing.Role == FederationRoleSpoke && existing.HubProjectUID == p.HubProjectUID {
			if err := tx.Commit(); err != nil {
				return AdoptProjectIntoFederationResult{}, fmt.Errorf("commit idempotent federation adoption: %w", err)
			}
			return AdoptProjectIntoFederationResult{
				Project: project,
				Binding: existing,
			}, nil
		}
		return AdoptProjectIntoFederationResult{}, fmt.Errorf("project %d already has %q federation binding", p.ProjectID, existing.Role)
	}
	if !errors.Is(err, ErrNotFound) {
		return AdoptProjectIntoFederationResult{}, err
	}

	issues, err := federationIssuesForSnapshot(ctx, tx, project.ID)
	if err != nil {
		return AdoptProjectIntoFederationResult{}, err
	}

	pushFloor, err := federationAdoptionPushFloor(ctx, tx, project.ID, d.InstanceUID())
	if err != nil {
		return AdoptProjectIntoFederationResult{}, err
	}

	if project.UID != p.HubProjectUID {
		if err := replaceProjectUIDTx(ctx, tx, project.ID, p.HubProjectUID); err != nil {
			return AdoptProjectIntoFederationResult{}, err
		}
		project.UID = p.HubProjectUID
	}
	if err := clearProjectClaimStateTx(ctx, tx, project.ID); err != nil {
		return AdoptProjectIntoFederationResult{}, err
	}
	boundary, err := nextEventHLC(ctx, tx, time.Now().UTC())
	if err != nil {
		return AdoptProjectIntoFederationResult{}, err
	}
	baselineCreatedAt := time.Now().UTC().Format(sqliteTimeFormat)
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id = ?`, project.ID); err != nil {
		return AdoptProjectIntoFederationResult{}, fmt.Errorf("delete pre-adoption local events: %w", err)
	}

	pullCursor := p.ReplayHorizonEventID - 1
	if pullCursor < 0 {
		pullCursor = 0
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO federation_bindings(
			project_id, role, hub_url, hub_project_id, hub_project_uid,
			replay_horizon_event_id, pull_cursor_event_id, push_enabled,
			push_cursor_event_id, enabled
		)
		VALUES(?, ?, ?, ?, ?, ?, ?, 1, ?, 1)`,
		project.ID, string(FederationRoleSpoke), p.HubURL, p.HubProjectID, p.HubProjectUID,
		p.ReplayHorizonEventID, pullCursor, pushFloor); err != nil {
		return AdoptProjectIntoFederationResult{}, fmt.Errorf("insert adoption federation binding: %w", err)
	}

	if len(project.Metadata) > 0 && string(project.Metadata) != "{}" {
		payload, err := projectMetadataAdoptionPayload(project.Metadata)
		if err != nil {
			return AdoptProjectIntoFederationResult{}, err
		}
		if _, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID:   project.ID,
			ProjectUID:  project.UID,
			ProjectName: project.Name,
			Type:        "project.metadata_updated",
			Actor:       actor,
			Payload:     payload,
			HLC:         &boundary,
			CreatedAt:   baselineCreatedAt,
		}); err != nil {
			return AdoptProjectIntoFederationResult{}, err
		}
	}

	var snapshotCount int64
	for _, issue := range issues {
		payload, err := d.federationIssueSnapshotPayload(ctx, tx, issue)
		if err != nil {
			return AdoptProjectIntoFederationResult{}, err
		}
		issueUID := issue.UID
		if _, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID:   project.ID,
			ProjectUID:  project.UID,
			ProjectName: project.Name,
			IssueID:     &issue.ID,
			IssueUID:    &issueUID,
			Type:        "issue.snapshot",
			Actor:       actor,
			Payload:     payload,
			HLC:         &boundary,
			CreatedAt:   baselineCreatedAt,
		}); err != nil {
			return AdoptProjectIntoFederationResult{}, err
		}
		snapshotCount++
	}

	binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id = ?`, project.ID))
	if err != nil {
		return AdoptProjectIntoFederationResult{}, err
	}
	project, err = scanProject(tx.QueryRowContext(ctx,
		projectSelect+` WHERE id = ?`, project.ID))
	if err != nil {
		return AdoptProjectIntoFederationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AdoptProjectIntoFederationResult{}, fmt.Errorf("commit federation adoption: %w", err)
	}
	return AdoptProjectIntoFederationResult{
		Project:               project,
		Binding:               binding,
		AdoptionSnapshotCount: snapshotCount,
	}, nil
}

// EnableProjectFederation marks a local project as a pull hub and writes a
// replay baseline. The project.federation_enabled event is the replay horizon;
// issue.snapshot events immediately after it carry the current issue state.
func (d *DB) EnableProjectFederation(ctx context.Context, projectID int64, actor string) (FederationBinding, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return FederationBinding{}, fmt.Errorf("begin federation enable: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	project, err := scanProject(tx.QueryRowContext(ctx,
		projectSelect+` WHERE id = ? AND deleted_at IS NULL`, projectID))
	if err != nil {
		return FederationBinding{}, err
	}

	existing, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id = ?`, projectID))
	if err == nil {
		if existing.Role != FederationRoleHub {
			return FederationBinding{}, fmt.Errorf("project %d already has %q federation binding", projectID, existing.Role)
		}
		if err := tx.Commit(); err != nil {
			return FederationBinding{}, fmt.Errorf("commit existing federation binding: %w", err)
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return FederationBinding{}, err
	}

	enableEvent, err := d.insertFederationBaselineEventsTx(ctx, tx, project, actor)
	if err != nil {
		return FederationBinding{}, err
	}
	pullCursor := enableEvent.ID - 1
	if pullCursor < 0 {
		pullCursor = 0
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO federation_bindings(
			project_id, role, hub_url, hub_project_id, hub_project_uid,
			replay_horizon_event_id, pull_cursor_event_id, enabled
		)
		VALUES(?, ?, '', 0, ?, ?, ?, 1)`,
		project.ID, string(FederationRoleHub), project.UID, enableEvent.ID, pullCursor); err != nil {
		return FederationBinding{}, fmt.Errorf("insert federation binding: %w", err)
	}

	binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id = ?`, project.ID))
	if err != nil {
		return FederationBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return FederationBinding{}, fmt.Errorf("commit federation enable: %w", err)
	}
	return binding, nil
}

// RefreshProjectFederationBaseline writes a fresh replay baseline for an
// already-enabled hub project. It is used after hub-side purge reset boundaries
// so spokes that re-bootstrap do not lose still-live pre-purge issues.
func (d *DB) RefreshProjectFederationBaseline(
	ctx context.Context,
	projectID int64,
	actor string,
) (FederationBinding, bool, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return FederationBinding{}, false, fmt.Errorf("begin federation baseline refresh: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	project, err := scanProject(tx.QueryRowContext(ctx,
		projectSelect+` WHERE id = ? AND deleted_at IS NULL`, projectID))
	if err != nil {
		return FederationBinding{}, false, err
	}
	existing, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id = ?`, projectID))
	if errors.Is(err, ErrNotFound) {
		if err := tx.Commit(); err != nil {
			return FederationBinding{}, false, fmt.Errorf("commit empty federation baseline refresh: %w", err)
		}
		return FederationBinding{}, false, nil
	}
	if err != nil {
		return FederationBinding{}, false, err
	}
	if existing.Role != FederationRoleHub || !existing.Enabled {
		if err := tx.Commit(); err != nil {
			return FederationBinding{}, false, fmt.Errorf("commit skipped federation baseline refresh: %w", err)
		}
		return existing, false, nil
	}

	enableEvent, err := d.insertFederationBaselineEventsTx(ctx, tx, project, actor)
	if err != nil {
		return FederationBinding{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE federation_bindings
		   SET replay_horizon_event_id = ?,
		       pull_cursor_event_id = ?,
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE project_id = ?`,
		enableEvent.ID, enableEvent.ID-1, project.ID); err != nil {
		return FederationBinding{}, false, fmt.Errorf("update refreshed federation baseline: %w", err)
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id = ?`, projectID))
	if err != nil {
		return FederationBinding{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return FederationBinding{}, false, fmt.Errorf("commit federation baseline refresh: %w", err)
	}
	return binding, true, nil
}

func (d *DB) insertFederationBaselineEventsTx(
	ctx context.Context,
	tx *sql.Tx,
	project Project,
	actor string,
) (Event, error) {
	enablePayload, err := json.Marshal(struct {
		ProjectUID  string   `json:"project_uid"`
		ProjectName string   `json:"project_name"`
		Metadata    JSONBlob `json:"metadata"`
	}{
		ProjectUID:  project.UID,
		ProjectName: project.Name,
		Metadata:    project.Metadata,
	})
	if err != nil {
		return Event{}, fmt.Errorf("marshal federation enable payload: %w", err)
	}
	enableEvent, err := d.insertEventTx(ctx, tx, eventInsert{
		ProjectID:   project.ID,
		ProjectUID:  project.UID,
		ProjectName: project.Name,
		Type:        "project.federation_enabled",
		Actor:       actor,
		Payload:     string(enablePayload),
	})
	if err != nil {
		return Event{}, err
	}
	boundary := eventHLCTimestamp{
		PhysicalMS: enableEvent.HLCPhysicalMS,
		Counter:    enableEvent.HLCCounter,
	}
	baselineCreatedAt := enableEvent.CreatedAt.UTC().Format(sqliteTimeFormat)

	issues, err := federationIssuesForSnapshot(ctx, tx, project.ID)
	if err != nil {
		return Event{}, err
	}
	for _, issue := range issues {
		payload, err := d.federationIssueSnapshotPayload(ctx, tx, issue)
		if err != nil {
			return Event{}, err
		}
		issueUID := issue.UID
		if _, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID:   project.ID,
			ProjectUID:  project.UID,
			ProjectName: project.Name,
			IssueID:     &issue.ID,
			IssueUID:    &issueUID,
			Type:        "issue.snapshot",
			Actor:       actor,
			Payload:     payload,
			HLC:         &boundary,
			CreatedAt:   baselineCreatedAt,
		}); err != nil {
			return Event{}, err
		}
	}
	return enableEvent, nil
}

// MaterializeFederatedProject rebuilds the local read model for a spoke project
// from its stored remote events. Event rows are retained.
func (d *DB) MaterializeFederatedProject(ctx context.Context, projectID int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin federated materialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := d.materializeFederatedProjectTx(ctx, tx, projectID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit federated materialization: %w", err)
	}
	return nil
}

func (d *DB) materializeFederatedProjectTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id = ?`, projectID))
	if err != nil {
		return err
	}

	events, err := federationFoldEvents(ctx, tx, projectID)
	if err != nil {
		return err
	}
	projection := FoldEvents(events)
	issueIDs, err := reconcileFederatedIssues(ctx, tx, projectID, projection)
	if err != nil {
		return err
	}
	if err := reconcileFederatedComments(ctx, tx, projectID, issueIDs, projection); err != nil {
		return err
	}
	if err := reconcileFederatedLabels(ctx, tx, projectID, issueIDs, projection); err != nil {
		return err
	}
	if err := reconcileFederatedLinks(ctx, tx, projectID, issueIDs, projection); err != nil {
		return err
	}
	if err := pruneFederatedIssues(ctx, tx, projectID, issueIDs); err != nil {
		return err
	}
	if raw := projection.ProjectMetadata[binding.HubProjectUID]; len(raw) > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects
			    SET metadata = ?, revision = revision + 1
			  WHERE id = ? AND metadata IS NOT ?`,
			string(raw), projectID, string(raw)); err != nil {
			return fmt.Errorf("update federated project metadata: %w", err)
		}
	}
	return nil
}

// ResetFederatedProject clears a spoke project's pulled event/projection state
// and rewinds its binding to the supplied hub horizon cursor.
func (d *DB) ResetFederatedProject(ctx context.Context, projectID, replayHorizonEventID, pullCursorEventID int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin federated reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("clear federated events: %w", err)
	}
	if err := clearFederatedProjection(ctx, tx, projectID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE federation_bindings
		   SET replay_horizon_event_id = ?,
		       pull_cursor_event_id = ?,
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE project_id = ?`,
		replayHorizonEventID, pullCursorEventID, projectID)
	if err != nil {
		return fmt.Errorf("update federation reset cursor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update federation reset cursor rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit federated reset: %w", err)
	}
	return nil
}

func ensureProjectWritableTx(ctx context.Context, q sqlReader, projectID int64) error {
	var (
		role        string
		enabled     int
		pushEnabled int
	)
	err := q.QueryRowContext(ctx,
		`SELECT role, enabled, push_enabled FROM federation_bindings WHERE project_id = ?`, projectID).
		Scan(&role, &enabled, &pushEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check federation write gate: %w", err)
	}
	if enabled == 1 && role == string(FederationRoleSpoke) && pushEnabled != 1 {
		return ErrFederatedReadOnly
	}
	return nil
}

func ensureFederatedSpokeUnsupportedTx(ctx context.Context, q sqlReader, projectID int64) error {
	var role string
	var enabled int
	err := q.QueryRowContext(ctx,
		`SELECT role, enabled FROM federation_bindings WHERE project_id = ?`, projectID).
		Scan(&role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check federated spoke operation gate: %w", err)
	}
	if enabled == 1 && role == string(FederationRoleSpoke) {
		return errors.Join(ErrFederatedReadOnly, ErrFederatedSpokeUnsupported)
	}
	return nil
}

func ensureFederatedMoveAllowedTx(ctx context.Context, q sqlReader, projectIDs ...int64) error {
	for _, projectID := range projectIDs {
		var exists int
		err := q.QueryRowContext(ctx,
			`SELECT 1 FROM federation_bindings WHERE project_id = ?`, projectID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check federated move gate: %w", err)
		}
		return errors.Join(ErrFederatedReadOnly, ErrFederatedMoveUnsupported)
	}
	return nil
}

const federationBindingSelect = `SELECT project_id, role, hub_url, hub_project_id, hub_project_uid,
	       replay_horizon_event_id, pull_cursor_event_id, push_enabled, push_cursor_event_id,
	       enabled, created_at, updated_at, last_sync_at
	  FROM federation_bindings`

func scanFederationBinding(r rowScanner) (FederationBinding, error) {
	var (
		b           FederationBinding
		role        string
		enabled     int
		pushEnabled int
		lastSyncAt  sql.NullTime
	)
	err := r.Scan(&b.ProjectID, &role, &b.HubURL, &b.HubProjectID, &b.HubProjectUID,
		&b.ReplayHorizonEventID, &b.PullCursorEventID, &pushEnabled,
		&b.PushCursorEventID, &enabled, &b.CreatedAt, &b.UpdatedAt, &lastSyncAt)
	if err == nil {
		b.Role = FederationRole(role)
		b.PushEnabled = pushEnabled == 1
		b.Enabled = enabled == 1
		if lastSyncAt.Valid {
			b.LastSyncAt = &lastSyncAt.Time
		}
		return b, nil
	}
	if err == sql.ErrNoRows {
		return FederationBinding{}, ErrNotFound
	}
	return FederationBinding{}, fmt.Errorf("scan federation binding: %w", err)
}

func scanFederationSyncStatus(r rowScanner) (FederationSyncStatus, error) {
	var (
		status          FederationSyncStatus
		lastPullStarted sql.NullTime
		lastPullSuccess sql.NullTime
		lastPushStarted sql.NullTime
		lastPushSuccess sql.NullTime
		lastErrorAt     sql.NullTime
		lastError       sql.NullString
		lastResetAt     sql.NullTime
	)
	err := r.Scan(&status.ProjectID, &lastPullStarted, &lastPullSuccess,
		&lastPushStarted, &lastPushSuccess, &lastErrorAt, &lastError, &lastResetAt)
	if err == nil {
		if lastPullStarted.Valid {
			status.LastPullStartedAt = &lastPullStarted.Time
		}
		if lastPullSuccess.Valid {
			status.LastPullSuccessAt = &lastPullSuccess.Time
		}
		if lastPushStarted.Valid {
			status.LastPushStartedAt = &lastPushStarted.Time
		}
		if lastPushSuccess.Valid {
			status.LastPushSuccessAt = &lastPushSuccess.Time
		}
		if lastErrorAt.Valid {
			status.LastErrorAt = &lastErrorAt.Time
		}
		if lastError.Valid {
			status.LastError = &lastError.String
		}
		if lastResetAt.Valid {
			status.LastResetAt = &lastResetAt.Time
		}
		return status, nil
	}
	if err == sql.ErrNoRows {
		return FederationSyncStatus{}, ErrNotFound
	}
	return FederationSyncStatus{}, fmt.Errorf("scan federation sync status: %w", err)
}

const federationQuarantineSelect = `SELECT id, project_id, direction, first_event_id, last_event_id,
       event_uids, error, created_at, skipped_at, skipped_by, skip_reason
  FROM federation_quarantine`

func scanFederationQuarantines(rows *sql.Rows) ([]FederationQuarantine, error) {
	out := []FederationQuarantine{}
	for rows.Next() {
		q, err := scanFederationQuarantine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate federation quarantines: %w", err)
	}
	return out, nil
}

func scanFederationQuarantine(r rowScanner) (FederationQuarantine, error) {
	var (
		q          FederationQuarantine
		direction  string
		eventUIDs  string
		skippedAt  sql.NullTime
		skippedBy  sql.NullString
		skipReason sql.NullString
	)
	err := r.Scan(&q.ID, &q.ProjectID, &direction, &q.FirstEventID, &q.LastEventID,
		&eventUIDs, &q.Error, &q.CreatedAt, &skippedAt, &skippedBy, &skipReason)
	if err == nil {
		q.Direction = FederationQuarantineDirection(direction)
		if err := json.Unmarshal([]byte(eventUIDs), &q.EventUIDs); err != nil {
			return FederationQuarantine{}, fmt.Errorf("decode federation quarantine event uids: %w", err)
		}
		if q.EventUIDs == nil {
			q.EventUIDs = []string{}
		}
		if skippedAt.Valid {
			q.SkippedAt = &skippedAt.Time
		}
		if skippedBy.Valid {
			q.SkippedBy = &skippedBy.String
		}
		if skipReason.Valid {
			q.SkipReason = &skipReason.String
		}
		return q, nil
	}
	if err == sql.ErrNoRows {
		return FederationQuarantine{}, ErrNotFound
	}
	return FederationQuarantine{}, fmt.Errorf("scan federation quarantine: %w", err)
}

func validateFederationQuarantine(p RecordFederationQuarantineParams) error {
	if p.ProjectID <= 0 {
		return fmt.Errorf("federation quarantine project id is required")
	}
	if p.Direction != FederationQuarantineDirectionPush && p.Direction != FederationQuarantineDirectionPull {
		return fmt.Errorf("federation quarantine direction must be push or pull")
	}
	if p.FirstEventID < 0 || p.LastEventID < p.FirstEventID {
		return fmt.Errorf("federation quarantine event id range is invalid")
	}
	if strings.TrimSpace(p.Error) == "" {
		return fmt.Errorf("federation quarantine error is required")
	}
	return nil
}

func federationAdoptionPushFloor(ctx context.Context, tx *sql.Tx, projectID int64, originInstanceUID string) (int64, error) {
	var maxID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(id)
		  FROM events
		 WHERE project_id = ?
		   AND origin_instance_uid = ?
		   AND `+federationPushEventTypeCondition("type"),
		projectID, originInstanceUID).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("capture adoption push cursor floor: %w", err)
	}
	if maxID.Valid {
		return maxID.Int64, nil
	}
	return 0, nil
}

func replaceProjectUIDTx(ctx context.Context, tx *sql.Tx, projectID int64, uid string) error {
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS trg_projects_uid_immutable`); err != nil {
		return fmt.Errorf("drop project uid immutability trigger for adoption: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET uid = ? WHERE id = ?`, uid, projectID); err != nil {
		return fmt.Errorf("update adopted project uid: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TRIGGER trg_projects_uid_immutable
		BEFORE UPDATE OF uid ON projects
		FOR EACH ROW BEGIN
		  SELECT RAISE(ABORT, 'projects.uid is immutable')
		  WHERE NEW.uid <> OLD.uid;
		END`); err != nil {
		return fmt.Errorf("restore project uid immutability trigger after adoption: %w", err)
	}
	return nil
}

func projectMetadataAdoptionPayload(metadata JSONBlob) (string, error) {
	current := map[string]json.RawMessage{}
	if len(metadata) > 0 {
		if err := json.Unmarshal([]byte(metadata), &current); err != nil {
			return "", fmt.Errorf("decode adopted project metadata: %w", err)
		}
	}
	type diffEntry struct {
		From any             `json:"from"`
		To   json.RawMessage `json:"to"`
	}
	diff := make(map[string]diffEntry, len(current))
	for key, value := range current {
		diff[key] = diffEntry{From: nil, To: value}
	}
	payload, err := json.Marshal(struct {
		Diff map[string]diffEntry `json:"diff"`
	}{
		Diff: diff,
	})
	if err != nil {
		return "", fmt.Errorf("marshal adopted project metadata event: %w", err)
	}
	return string(payload), nil
}

func federationIssuesForSnapshot(ctx context.Context, tx *sql.Tx, projectID int64) ([]Issue, error) {
	rows, err := tx.QueryContext(ctx,
		issueSelect+` WHERE i.project_id = ? ORDER BY i.id ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list federation snapshot issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *DB) federationIssueSnapshotPayload(ctx context.Context, tx *sql.Tx, issue Issue) (string, error) {
	labels, err := federationIssueLabels(ctx, tx, issue.ID)
	if err != nil {
		return "", err
	}
	links, err := federationIssueLinks(ctx, tx, issue.ID)
	if err != nil {
		return "", err
	}
	comments, err := federationIssueComments(ctx, tx, issue.ID)
	if err != nil {
		return "", err
	}
	recurrenceUID, err := federationIssueRecurrenceUID(ctx, tx, issue.RecurrenceID)
	if err != nil {
		return "", err
	}
	occurrenceKey := ""
	if issue.OccurrenceKey != nil {
		occurrenceKey = *issue.OccurrenceKey
	}
	return buildIssueCreatedPayload(issueCreatedPayload{
		UID:           issue.UID,
		ShortID:       issue.ShortID,
		Title:         issue.Title,
		Body:          issue.Body,
		Author:        issue.Author,
		Owner:         issue.Owner,
		Priority:      issue.Priority,
		Status:        issue.Status,
		ClosedReason:  issue.ClosedReason,
		ClosedAt:      formatOptionalSQLiteTime(issue.ClosedAt),
		DeletedAt:     formatOptionalSQLiteTime(issue.DeletedAt),
		Metadata:      json.RawMessage(issue.Metadata),
		Labels:        labels,
		Links:         links,
		Comments:      comments,
		CreatedAt:     issue.CreatedAt.UTC().Format(sqliteTimeFormat),
		UpdatedAt:     issue.UpdatedAt.UTC().Format(sqliteTimeFormat),
		Revision:      issue.Revision,
		RecurrenceUID: recurrenceUID,
		OccurrenceKey: occurrenceKey,
	})
}

func federationIssueLabels(ctx context.Context, tx *sql.Tx, issueID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT label FROM issue_labels WHERE issue_id = ? ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list federation snapshot labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("scan federation snapshot label: %w", err)
		}
		out = append(out, label)
	}
	return out, rows.Err()
}

func federationIssueLinks(ctx context.Context, tx *sql.Tx, issueID int64) ([]createdLinkOut, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT l.type, peer.short_id, peer.uid, subject.project_id, peer.project_id
		  FROM links l
		  JOIN issues subject ON subject.id = l.from_issue_id
		  JOIN issues peer ON peer.id = l.to_issue_id
		 WHERE l.from_issue_id = ?
		 ORDER BY l.type ASC, peer.uid ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list federation snapshot links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []createdLinkOut
	for rows.Next() {
		var (
			link                        createdLinkOut
			subjectProject, peerProject int64
		)
		if err := rows.Scan(&link.Type, &link.ToShortID, &link.ToIssueUID, &subjectProject, &peerProject); err != nil {
			return nil, fmt.Errorf("scan federation snapshot link: %w", err)
		}
		if subjectProject != peerProject {
			return nil, fmt.Errorf("out-of-project link in federation snapshot for issue %d", issueID)
		}
		out = append(out, link)
	}
	return out, rows.Err()
}

func federationIssueComments(ctx context.Context, tx *sql.Tx, issueID int64) ([]issueSnapshotComment, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT uid, author, body, created_at
		  FROM comments
		 WHERE issue_id = ?
		 ORDER BY id ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list federation snapshot comments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []issueSnapshotComment
	for rows.Next() {
		var (
			comment   issueSnapshotComment
			createdAt sql.NullTime
		)
		if err := rows.Scan(&comment.CommentUID, &comment.Author, &comment.Body, &createdAt); err != nil {
			return nil, fmt.Errorf("scan federation snapshot comment: %w", err)
		}
		if createdAt.Valid {
			comment.CreatedAt = createdAt.Time.UTC().Format(sqliteTimeFormat)
		}
		out = append(out, comment)
	}
	return out, rows.Err()
}

func federationIssueRecurrenceUID(ctx context.Context, tx *sql.Tx, recurrenceID *int64) (string, error) {
	if recurrenceID == nil {
		return "", nil
	}
	var uid string
	if err := tx.QueryRowContext(ctx,
		`SELECT uid FROM recurrences WHERE id = ?`, *recurrenceID).Scan(&uid); err != nil {
		return "", fmt.Errorf("lookup federation snapshot recurrence uid: %w", err)
	}
	return uid, nil
}

func federationFoldEvents(ctx context.Context, tx *sql.Tx, projectID int64) ([]FoldEvent, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT uid, origin_instance_uid, project_name, issue_uid, related_issue_uid,
		       type, actor, payload, hlc_physical_ms, hlc_counter, created_at
		  FROM events
		 WHERE project_id = ?
		 ORDER BY id ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list federated events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	projectUID, err := projectUIDTx(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	var out []FoldEvent
	for rows.Next() {
		var (
			e               FoldEvent
			projectName     string
			payload         string
			issueUID        sql.NullString
			relatedIssueUID sql.NullString
			createdAt       time.Time
		)
		if err := rows.Scan(&e.UID, &e.OriginInstanceUID, &projectName, &issueUID,
			&relatedIssueUID, &e.Type, &e.Actor, &payload, &e.HLCPhysicalMS,
			&e.HLCCounter, &createdAt); err != nil {
			return nil, fmt.Errorf("scan federated event: %w", err)
		}
		e.ProjectUID = projectUID
		if issueUID.Valid {
			e.IssueUID = issueUID.String
		}
		if relatedIssueUID.Valid {
			e.RelatedIssueUID = relatedIssueUID.String
		}
		e.Payload = json.RawMessage(payload)
		e.CreatedAt = createdAt.UTC().Format(sqliteTimeFormat)
		out = append(out, e)
	}
	return out, rows.Err()
}

func projectUIDTx(ctx context.Context, tx *sql.Tx, projectID int64) (string, error) {
	var uid string
	if err := tx.QueryRowContext(ctx,
		`SELECT uid FROM projects WHERE id = ?`, projectID).Scan(&uid); err != nil {
		return "", fmt.Errorf("lookup project uid: %w", err)
	}
	return uid, nil
}

func clearFederatedProjection(ctx context.Context, tx *sql.Tx, projectID int64) error {
	if err := clearProjectClaimStateTx(ctx, tx, projectID); err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM issue_labels WHERE issue_id IN (SELECT id FROM issues WHERE project_id = ?)`,
		`DELETE FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id = ?)`,
		`DELETE FROM links WHERE project_id = ?`,
		`DELETE FROM issues WHERE project_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, projectID); err != nil {
			return fmt.Errorf("clear federated projection: %w", err)
		}
	}
	return nil
}

func clearProjectClaimStateTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	for _, stmt := range []string{
		`DELETE FROM pending_claim_requests WHERE project_id = ?`,
		`DELETE FROM issue_claims WHERE project_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, projectID); err != nil {
			return fmt.Errorf("clear project claim state: %w", err)
		}
	}
	return nil
}

func reconcileFederatedIssues(
	ctx context.Context, tx *sql.Tx, projectID int64, projection FoldProjection,
) (map[string]int64, error) {
	existing, err := federatedIssueRowsByUID(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(projection.Issues))
	for uid := range projection.Issues {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := map[string]int64{}
	for _, uid := range uids {
		issue := projection.Issues[uid]
		var existingRow *federatedIssueRow
		if row, ok := existing[uid]; ok {
			existingRow = &row
		}
		shortID, err := resolveFederatedIssueShortID(ctx, tx, projectID, issue.UID, issue.ShortID, existingRow)
		if err != nil {
			return nil, fmt.Errorf("resolve federated issue short_id %s: %w", uid, err)
		}
		metadata := json.RawMessage(`{}`)
		if raw := projection.IssueMetadata[uid]; len(raw) > 0 {
			metadata = raw
		}
		updatedAt := issue.UpdatedAt
		if updatedAt == "" {
			updatedAt = issue.CreatedAt
		}
		if row, ok := existing[uid]; ok {
			issueValues := []any{
				shortID, issue.Title, issue.Body, nonEmptyStatus(issue.Status),
				issue.ClosedReason, issue.Owner, issue.Priority, nonEmptyAuthor(issue.Author),
				nonEmptyTime(issue.CreatedAt), nonEmptyTime(updatedAt),
				optionalStringValue(issue.ClosedAt), optionalStringValue(issue.DeletedAt),
				string(metadata),
			}
			args := append([]any{}, issueValues...)
			args = append(args, row.id)
			args = append(args, issueValues...)
			_, err := tx.ExecContext(ctx, `
					UPDATE issues
					   SET short_id = ?,
					       title = ?,
				       body = ?,
				       status = ?,
				       closed_reason = ?,
				       owner = ?,
				       priority = ?,
				       author = ?,
				       created_at = ?,
				       updated_at = ?,
				       closed_at = ?,
					       deleted_at = ?,
					       metadata = ?,
					       revision = revision + 1
					 WHERE id = ?
					   AND (
					       short_id IS NOT ? OR
					       title IS NOT ? OR
					       body IS NOT ? OR
					       status IS NOT ? OR
					       closed_reason IS NOT ? OR
					       owner IS NOT ? OR
					       priority IS NOT ? OR
					       author IS NOT ? OR
					       created_at IS NOT ? OR
					       updated_at IS NOT ? OR
					       closed_at IS NOT ? OR
					       deleted_at IS NOT ? OR
					       metadata IS NOT ?
					   )`,
				args...)
			if err != nil {
				return nil, fmt.Errorf("update federated issue %s: %w", uid, err)
			}
			out[uid] = row.id
			continue
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO issues(
				uid, project_id, short_id, title, body, status, closed_reason,
				owner, priority, author, created_at, updated_at, closed_at,
				deleted_at, metadata, revision
			)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
			issue.UID, projectID, shortID, issue.Title, issue.Body, nonEmptyStatus(issue.Status),
			issue.ClosedReason, issue.Owner, issue.Priority, nonEmptyAuthor(issue.Author),
			nonEmptyTime(issue.CreatedAt), nonEmptyTime(updatedAt),
			optionalStringValue(issue.ClosedAt), optionalStringValue(issue.DeletedAt),
			string(metadata))
		if err != nil {
			return nil, fmt.Errorf("insert federated issue %s: %w", uid, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		out[uid] = id
	}
	return out, nil
}

func resolveFederatedIssueShortID(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	issueUID string,
	preferred string,
	existing *federatedIssueRow,
) (string, error) {
	if preferred == "" && existing != nil {
		return existing.shortID, nil
	}
	minLength := shortid.MinLength
	if preferred != "" {
		if !shortid.Valid(preferred) {
			return "", fmt.Errorf("invalid federated short_id %q", preferred)
		}
		derived, err := shortid.Derive(issueUID, len(preferred))
		if err != nil {
			return "", fmt.Errorf("validate federated short_id %q against uid %q: %w", preferred, issueUID, err)
		}
		if preferred != derived {
			return "", fmt.Errorf("federated short_id %q does not match uid %q suffix at length %d (expected %q)",
				preferred, issueUID, len(preferred), derived)
		}
		minLength = len(preferred)
	}
	if existing != nil && len(existing.shortID) > minLength {
		minLength = len(existing.shortID)
	}
	shortID, err := assignShortIDIn(ctx, tx, []int64{projectID}, issueUID, minLength)
	if err != nil {
		return "", fmt.Errorf("assign federated short_id: %w", err)
	}
	return shortID, nil
}

type federatedIssueRow struct {
	id      int64
	shortID string
}

func federatedIssueRowsByUID(ctx context.Context, tx *sql.Tx, projectID int64) (map[string]federatedIssueRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT uid, id, short_id FROM issues WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list federated issue rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]federatedIssueRow{}
	for rows.Next() {
		var uid string
		var row federatedIssueRow
		if err := rows.Scan(&uid, &row.id, &row.shortID); err != nil {
			return nil, fmt.Errorf("scan federated issue row: %w", err)
		}
		out[uid] = row
	}
	return out, rows.Err()
}

func reconcileFederatedComments(
	ctx context.Context, tx *sql.Tx, projectID int64, issueIDs map[string]int64, projection FoldProjection,
) error {
	existing, err := federatedCommentIDsByUID(ctx, tx, projectID)
	if err != nil {
		return err
	}
	desired := map[string]struct{}{}
	uids := make([]string, 0, len(projection.Comments))
	for uid := range projection.Comments {
		uids = append(uids, uid)
		desired[uid] = struct{}{}
	}
	sort.Strings(uids)
	for uid, id := range existing {
		if _, ok := desired[uid]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM comments WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete stale federated comment %s: %w", uid, err)
		}
	}
	for _, uid := range uids {
		comment := projection.Comments[uid]
		issueID, ok := issueIDs[comment.IssueUID]
		if !ok {
			return fmt.Errorf("federated comment %s references unknown issue %s", uid, comment.IssueUID)
		}
		if id, ok := existing[uid]; ok {
			if _, err := tx.ExecContext(ctx,
				`UPDATE comments SET issue_id = ?, author = ?, body = ?, created_at = ? WHERE id = ?`,
				issueID, nonEmptyAuthor(comment.Author), comment.Body, nonEmptyTime(comment.CreatedAt), id); err != nil {
				return fmt.Errorf("update federated comment %s: %w", uid, err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO comments(uid, issue_id, author, body, created_at) VALUES(?, ?, ?, ?, ?)`,
			comment.UID, issueID, nonEmptyAuthor(comment.Author), comment.Body, nonEmptyTime(comment.CreatedAt)); err != nil {
			return fmt.Errorf("insert federated comment %s: %w", uid, err)
		}
	}
	return nil
}

func federatedCommentIDsByUID(ctx context.Context, tx *sql.Tx, projectID int64) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.uid, c.id
		  FROM comments c
		  JOIN issues i ON i.id = c.issue_id
		 WHERE i.project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list federated comments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var uid string
		var id int64
		if err := rows.Scan(&uid, &id); err != nil {
			return nil, fmt.Errorf("scan federated comment: %w", err)
		}
		out[uid] = id
	}
	return out, rows.Err()
}

type federatedLabelKey struct {
	issueID int64
	label   string
}

func reconcileFederatedLabels(
	ctx context.Context, tx *sql.Tx, projectID int64, issueIDs map[string]int64, projection FoldProjection,
) error {
	existing, err := federatedLabelKeys(ctx, tx, projectID)
	if err != nil {
		return err
	}
	desired := map[federatedLabelKey]struct{}{}
	keys := make([]FoldLabelKey, 0, len(projection.Labels))
	for key, state := range projection.Labels {
		if state.Present {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].IssueUID != keys[j].IssueUID {
			return keys[i].IssueUID < keys[j].IssueUID
		}
		return keys[i].Label < keys[j].Label
	})
	for _, key := range keys {
		issueID, ok := issueIDs[key.IssueUID]
		if !ok {
			return fmt.Errorf("federated label %s references unknown issue %s", key.Label, key.IssueUID)
		}
		desired[federatedLabelKey{issueID: issueID, label: key.Label}] = struct{}{}
	}
	for key := range existing {
		if _, ok := desired[key]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM issue_labels WHERE issue_id = ? AND label = ?`, key.issueID, key.label); err != nil {
			return fmt.Errorf("delete stale federated label %s: %w", key.label, err)
		}
	}
	for key := range desired {
		if _, ok := existing[key]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO issue_labels(issue_id, label, author) VALUES(?, ?, 'federation')`,
			key.issueID, key.label); err != nil {
			return fmt.Errorf("insert federated label %s: %w", key.label, err)
		}
	}
	return nil
}

func federatedLabelKeys(ctx context.Context, tx *sql.Tx, projectID int64) (map[federatedLabelKey]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT il.issue_id, il.label
		  FROM issue_labels il
		  JOIN issues i ON i.id = il.issue_id
		 WHERE i.project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list federated labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[federatedLabelKey]struct{}{}
	for rows.Next() {
		var key federatedLabelKey
		if err := rows.Scan(&key.issueID, &key.label); err != nil {
			return nil, fmt.Errorf("scan federated label: %w", err)
		}
		out[key] = struct{}{}
	}
	return out, rows.Err()
}

type federatedLinkRow struct {
	id      int64
	fromID  int64
	toID    int64
	fromUID string
	toUID   string
	typ     string
}

func (r federatedLinkRow) key() FoldLinkKey {
	return FoldLinkKey{FromUID: r.fromUID, ToUID: r.toUID, Type: r.typ}
}

func reconcileFederatedLinks(ctx context.Context, tx *sql.Tx, projectID int64, issueIDs map[string]int64, projection FoldProjection) error {
	existing, err := federatedLinkRows(ctx, tx, projectID)
	if err != nil {
		return err
	}
	desired := map[FoldLinkKey]federatedLinkRow{}
	keys := make([]FoldLinkKey, 0, len(projection.Links))
	for key, state := range projection.Links {
		if state.Present {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].FromUID != keys[j].FromUID {
			return keys[i].FromUID < keys[j].FromUID
		}
		if keys[i].ToUID != keys[j].ToUID {
			return keys[i].ToUID < keys[j].ToUID
		}
		return keys[i].Type < keys[j].Type
	})
	for _, key := range keys {
		fromID, fromOK := issueIDs[key.FromUID]
		toID, toOK := issueIDs[key.ToUID]
		if !fromOK || !toOK {
			// A baseline can arrive over multiple poll pages. Skip incomplete
			// links for this materialization pass; a later pass sees both
			// snapshots and recreates the edge.
			continue
		}
		fromUID, toUID := key.FromUID, key.ToUID
		if key.Type == "related" && fromID > toID {
			fromID, toID = toID, fromID
			fromUID, toUID = toUID, fromUID
		}
		row := federatedLinkRow{
			fromID:  fromID,
			toID:    toID,
			fromUID: fromUID,
			toUID:   toUID,
			typ:     key.Type,
		}
		desired[row.key()] = row
	}
	for key, row := range existing {
		if _, ok := desired[key]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, row.id); err != nil {
			return fmt.Errorf("delete stale federated link %s %s->%s: %w", key.Type, key.FromUID, key.ToUID, err)
		}
	}
	for key, row := range desired {
		if existingRow, ok := existing[key]; ok {
			if existingRow.fromID == row.fromID && existingRow.toID == row.toID {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE links
				   SET project_id = ?, from_issue_id = ?, to_issue_id = ?,
				       from_issue_uid = ?, to_issue_uid = ?, type = ?
				 WHERE id = ?`,
				projectID, row.fromID, row.toID, row.fromUID, row.toUID, row.typ, existingRow.id); err != nil {
				return fmt.Errorf("update federated link %s %s->%s: %w", key.Type, key.FromUID, key.ToUID, err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
			VALUES(?, ?, ?, ?, ?, ?, 'federation')`,
			projectID, row.fromID, row.toID, row.fromUID, row.toUID, row.typ); err != nil {
			return fmt.Errorf("insert federated link %s %s->%s: %w", key.Type, key.FromUID, key.ToUID, err)
		}
	}
	return nil
}

func federatedLinkRows(ctx context.Context, tx *sql.Tx, projectID int64) (map[FoldLinkKey]federatedLinkRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type
		  FROM links
		 WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list federated links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[FoldLinkKey]federatedLinkRow{}
	for rows.Next() {
		var row federatedLinkRow
		if err := rows.Scan(&row.id, &row.fromID, &row.toID, &row.fromUID, &row.toUID, &row.typ); err != nil {
			return nil, fmt.Errorf("scan federated link: %w", err)
		}
		out[row.key()] = row
	}
	return out, rows.Err()
}

func pruneFederatedIssues(ctx context.Context, tx *sql.Tx, projectID int64, issueIDs map[string]int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, uid FROM issues WHERE project_id = ?`, projectID)
	if err != nil {
		return fmt.Errorf("list stale federated issues: %w", err)
	}
	var candidates []struct {
		id  int64
		uid string
	}
	for rows.Next() {
		var c struct {
			id  int64
			uid string
		}
		if err := rows.Scan(&c.id, &c.uid); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan stale federated issue: %w", err)
		}
		if _, ok := issueIDs[c.uid]; !ok {
			candidates = append(candidates, c)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close stale federated issues: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate stale federated issues: %w", err)
	}
	for _, c := range candidates {
		var refs int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*)
			  FROM events
			 WHERE issue_id = ? OR related_issue_id = ?`, c.id, c.id).Scan(&refs); err != nil {
			return fmt.Errorf("count federated issue event refs %s: %w", c.uid, err)
		}
		if refs > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM issues WHERE id = ?`, c.id); err != nil {
			return fmt.Errorf("delete stale federated issue %s: %w", c.uid, err)
		}
	}
	return nil
}

func nonEmptyTime(s string) string {
	if s != "" {
		return s
	}
	return "1970-01-01T00:00:00.000Z"
}

func nonEmptyStatus(s string) string {
	if s != "" {
		return s
	}
	return "open"
}

func nonEmptyAuthor(s string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return "federation"
}

func optionalStringValue(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}
