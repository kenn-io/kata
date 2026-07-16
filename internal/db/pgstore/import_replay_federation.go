package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"go.kenn.io/kata/internal/db"
)

func pgReplayFederationBinding(
	ctx context.Context,
	tx *sql.Tx,
	binding *db.FederationBindingExport,
) error {
	actor := strings.TrimSpace(binding.Actor)
	pushEnabled := binding.PushEnabled
	if binding.Role == string(db.FederationRoleSpoke) && pushEnabled && actor == "" {
		pushEnabled = false
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO federation_bindings(
project_id,role,hub_url,hub_project_id,hub_project_uid,replay_horizon_event_id,
pull_cursor_event_id,push_enabled,push_cursor_event_id,bound_actor,allow_insecure,
enabled,created_at,updated_at,last_sync_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		binding.ProjectID, binding.Role, binding.HubURL, binding.HubProjectID,
		binding.HubProjectUID, binding.ReplayHorizonEventID, binding.PullCursorEventID,
		pgReplayBoolInt(pushEnabled), binding.PushCursorEventID, actor,
		pgReplayBoolInt(binding.AllowInsecure), pgReplayBoolInt(binding.Enabled),
		binding.CreatedAt, binding.UpdatedAt, binding.LastSyncAt)
	return pgReplayError(db.ImportKindFederationBinding, err)
}

func pgReplayFederationSyncStatus(
	ctx context.Context,
	tx *sql.Tx,
	status *db.FederationSyncStatusExport,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO federation_sync_status(
project_id,last_pull_started_at,last_pull_success_at,last_push_started_at,
last_push_success_at,last_error_at,last_error,last_reset_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, status.ProjectID, status.LastPullStartedAt,
		status.LastPullSuccessAt, status.LastPushStartedAt, status.LastPushSuccessAt,
		status.LastErrorAt, status.LastError, status.LastResetAt)
	return pgReplayError(db.ImportKindFederationSyncStatus, err)
}

func pgReplayFederationQuarantine(
	ctx context.Context,
	tx *sql.Tx,
	quarantine *db.FederationQuarantineExport,
) error {
	eventUIDs := quarantine.EventUIDs
	if len(eventUIDs) == 0 {
		eventUIDs = json.RawMessage(`[]`)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO federation_quarantine(
id,project_id,direction,first_event_id,last_event_id,event_uids,error,created_at,
skipped_at,skipped_by,skip_reason
) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		quarantine.ID, quarantine.ProjectID, quarantine.Direction, quarantine.FirstEventID,
		quarantine.LastEventID, string(eventUIDs), quarantine.Error, quarantine.CreatedAt,
		quarantine.SkippedAt, quarantine.SkippedBy, quarantine.SkipReason)
	return pgReplayError(db.ImportKindFederationQuarantine, err)
}

func pgReplayFederationEnrollment(
	ctx context.Context,
	tx *sql.Tx,
	enrollment *db.FederationEnrollmentExport,
) error {
	actor := strings.TrimSpace(enrollment.Actor)
	capabilities := enrollment.Capabilities
	if actor == "" {
		actor = "legacy-federation"
		capabilities = "pull"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO federation_enrollments(
id,token_hash,spoke_instance_uid,project_id,capabilities,bound_actor,
allow_adoption_snapshot_authors,adoption_baseline_open,
adoption_baseline_next_source_event_id,adoption_baseline_end_source_event_id,
created_at,updated_at,revoked_at
) OVERRIDING SYSTEM VALUE VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		enrollment.ID, enrollment.TokenHash, enrollment.SpokeInstanceUID, enrollment.ProjectID,
		capabilities, actor, pgReplayBoolInt(enrollment.AllowAdoptionSnapshotAuthors),
		pgReplayBoolInt(enrollment.AdoptionBaselineOpen),
		enrollment.AdoptionBaselineNextSourceEventID,
		enrollment.AdoptionBaselineEndSourceEventID, enrollment.CreatedAt,
		enrollment.UpdatedAt, enrollment.RevokedAt)
	return pgReplayError(db.ImportKindFederationEnrollment, err)
}

func pgReplayBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
