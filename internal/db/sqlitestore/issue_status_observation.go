package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
)

var _ db.IssueStatusWriter = (*Store)(nil)

// ObserveIssueStatus commits the independent provider checkpoint, inward event,
// and exact-event acknowledgement under the existing binding claim fence.
func (s *Store) ObserveIssueStatus(ctx context.Context, p db.IssueStatusObservationParams) (bool, []db.Event, error) {
	return retryWrite2(ctx, s, func() (bool, []db.Event, error) {
		tx, err := s.BeginTx(ctx, nil)
		if err != nil {
			return false, nil, err
		}
		defer func() { _ = tx.Rollback() }()
		changed, events, err := s.observeIssueStatusTx(ctx, tx, p)
		if err != nil {
			return false, nil, err
		}
		if err := tx.Commit(); err != nil {
			return false, nil, err
		}
		return changed, events, nil
	})
}

func (s *Store) observeIssueStatusTx(ctx context.Context, tx *sql.Tx, p db.IssueStatusObservationParams) (bool, []db.Event, error) {
	binding, err := statusReadBindingTx(ctx, tx, p.Guard)
	if err != nil {
		return false, nil, err
	}
	current, err := loadIssueStatusMappingTx(ctx, tx, binding, p.MappingID)
	if err != nil {
		return false, nil, err
	}
	issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1 AND i.deleted_at IS NULL`, *current.Mapping.IssueID))
	if err != nil {
		return false, nil, err
	}
	var project db.Project
	project.ID = binding.ProjectID
	err = tx.QueryRowContext(ctx, `SELECT uid,name FROM projects WHERE id=$1`, binding.ProjectID).Scan(&project.UID, &project.Name)
	if err != nil {
		return false, nil, err
	}
	if current.Mapping.ExternalID != p.ExternalID || issue.UID != p.IssueUID || issue.ProjectID != binding.ProjectID {
		return false, nil, fmt.Errorf("%w: status observation identity changed", db.ErrImportValidation)
	}
	if p.ServicedEventUID != "" {
		var config struct {
			Mode string `json:"status_sync"`
		}
		if err := json.Unmarshal(binding.Config, &config); err != nil {
			return false, nil, err
		}
		if config.Mode != "two-way" {
			return false, nil, db.ErrIssueSyncNotEnabled
		}
	}
	accept, apply, err := db.PlanIssueStatusObservation(current, p, issue.Status)
	if err != nil || !accept {
		return false, nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE import_mappings SET observed_status=$1,observed_status_at=$2,
 remote_locator=COALESCE($3,remote_locator),pending_event_uid=CASE WHEN pending_event_uid=$4 THEN NULL ELSE pending_event_uid END WHERE id=$5`,
		p.Observation.Raw, p.Observation.Version.UTC().Format(time.RFC3339Nano), p.RemoteLocator, p.ServicedEventUID, p.MappingID)
	if err != nil {
		return false, nil, err
	}
	if !apply {
		return false, nil, nil
	}
	at := p.Observation.Version.UTC()
	// Status timestamps cannot violate the native creation/close invariant.
	if at.Before(issue.CreatedAt) {
		at = issue.CreatedAt
	}
	var reason, closedAt any
	if p.Status == "closed" {
		reason = "done"
		if p.ClosedReason != "" {
			reason = p.ClosedReason
		}
		closeTime := at
		if p.ClosedAt != nil && !p.ClosedAt.IsZero() && !p.ClosedAt.Before(issue.CreatedAt) {
			closeTime = p.ClosedAt.UTC()
		}
		closedAt = closeTime.Format(time.RFC3339Nano)
	}
	_, err = tx.ExecContext(ctx, `UPDATE issues SET status=$1,closed_reason=$2,closed_at=$3,
	 assignment_expires_on=CASE WHEN $1='closed' THEN NULL ELSE assignment_expires_on END,revision=revision+1 WHERE id=$4`, p.Status, reason, closedAt, issue.ID)
	if err != nil {
		return false, nil, err
	}
	payload := map[string]any{"status": p.Status, "closed_reason": reason, "closed_at": closedAt, "updated_at": issue.UpdatedAt.UTC().Format(time.RFC3339Nano), "source": binding.SourceKey, "external_id": p.ExternalID}
	if p.Status == "closed" {
		payload["assignment_expires_on"] = nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, nil, err
	}
	actorParams, err := s.normalizeBoundFederationImportActorTx(ctx, tx, db.ImportBatchParams{ProjectID: binding.ProjectID, Actor: binding.Provider + "-sync"})
	if err != nil {
		return false, nil, err
	}
	event, err := s.insertEventTx(ctx, tx, eventInsert{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, IssueID: &issue.ID, IssueUID: &issue.UID, Type: "issue.updated", Actor: actorParams.Actor, Payload: string(body)})
	if err != nil {
		return false, nil, err
	}
	return true, []db.Event{event}, nil
}
