package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"
)

// AuthorizeFederationCurrentIssue checks every issue affected by a fresh event.
func AuthorizeFederationCurrentIssue(ctx context.Context, tx *sql.Tx, p FederationIngestParams, event RemoteEvent, moveSnapshots *FederationMoveSnapshotValidator, staged map[string]int64, checked map[int64]bool, authorize func(context.Context, int64, string) error) error {
	referenceEvent, err := moveSnapshots.UnprovenReferences(ctx, event)
	if err != nil {
		return err
	}
	var primary string
	var refs []string
	switch {
	case strings.HasPrefix(event.Type, "issue."):
		payload := PayloadMap(event.Payload)
		var err error
		primary, err = FederationPayloadIssueUID(event, payload)
		if err != nil {
			return err
		}
		refs, err = FederationPayloadReferencedIssueUIDs(referenceEvent, PayloadMap(referenceEvent.Payload))
		if err != nil {
			return err
		}
	case event.Type == "cron.run.observed" || event.Type == "cron.run.snapshot":
		// A run's UID identifies the run, not its optional issue reference.
		var run CronRunObservation
		if err := json.Unmarshal(event.Payload, &run); err != nil {
			return err
		}
		if run.IssueUID != nil {
			primary = *run.IssueUID
		}
	default:
		return nil
	}
	if event.Type == "issue.moved" {
		replyIssues, err := FederationMoveSnapshotReplyIssues(ctx, tx, referenceEvent)
		if err != nil {
			return err
		}
		refs = append(refs, replyIssues...)
	}
	refs = append(refs, primary)
	for _, issueUID := range refs {
		if issueUID == "" {
			continue
		}
		current, known := staged[issueUID]
		if !known {
			err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=$1`, issueUID).Scan(&current)
			if errors.Is(err, sql.ErrNoRows) {
				// Existing validation permits deferred links and fresh creation.
				continue
			}
			if err != nil {
				return err
			}
			staged[issueUID] = current
		}
		if current == p.ProjectID || checked[current] {
			continue
		}
		var projectUID string
		err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 AND deleted_at IS NULL`, current).Scan(&projectUID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := authorize(ctx, current, projectUID); err != nil {
			if missing, ok := errors.AsType[*FederationProjectNotEnrolledError](err); ok {
				missing.IssueUID = issueUID
			}
			return err
		}
		checked[current] = true
	}
	switch event.Type {
	case "issue.created", "issue.snapshot":
		if _, exists := staged[primary]; !exists {
			staged[primary] = p.ProjectID
		}
	case "issue.moved":
		_, to, err := FederationMoveProjects(ctx, tx, p.ProjectID, event)
		if err != nil {
			return err
		}
		staged[primary] = to
	}
	return nil
}
