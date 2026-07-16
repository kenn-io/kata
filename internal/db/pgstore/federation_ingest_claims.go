package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/db"
)

type federationIngestClaimAuditIssue struct {
	UID          string
	RequireClaim bool
}

func (s *Store) annotateFederationIngestClaimWorkTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	event db.RemoteEvent,
) ([]db.Event, error) {
	issueUIDs, err := federationIngestClaimAuditIssueUIDs(event)
	if err != nil || len(issueUIDs) == 0 {
		return nil, err
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
		federationBindingSelect+` WHERE project_id=$1`, projectID))
	if err != nil {
		return nil, err
	}
	projectIDs, err := federationBindingGroupProjectIDs(ctx, tx, binding)
	if err != nil {
		return nil, err
	}
	var output []db.Event
	for _, candidate := range issueUIDs {
		issue, project, lookupErr := issueForClaimAuditInGroupTx(
			ctx, tx, projectIDs, candidate.UID,
		)
		if errors.Is(lookupErr, db.ErrNotFound) {
			continue
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		events, auditErr := s.annotateClaimWorkMutationTx(ctx, tx, claimWorkMutationInput{
			Project: project, Issue: issue, OffendingEventUID: event.EventUID,
			EventType: event.Type, Actor: event.Actor,
			HolderInstanceUID: event.OriginInstanceUID, RequireClaim: candidate.RequireClaim,
		})
		if auditErr != nil {
			return nil, auditErr
		}
		output = append(output, events...)
	}
	return output, nil
}

func federationIngestClaimAuditIssueUIDs(
	event db.RemoteEvent,
) ([]federationIngestClaimAuditIssue, error) {
	payload := db.PayloadMap(event.Payload)
	output := make([]federationIngestClaimAuditIssue, 0, 1)
	seen := map[string]struct{}{}
	add := func(candidate federationIngestClaimAuditIssue) {
		if candidate.UID == "" {
			return
		}
		if _, ok := seen[candidate.UID]; ok {
			return
		}
		seen[candidate.UID] = struct{}{}
		output = append(output, candidate)
	}
	if claimWorkMutationRequiresClaim(event.Type) || event.Type == "issue.snapshot" {
		issueUID, err := payloadIssueUID(event, payload)
		if err != nil {
			return nil, err
		}
		add(federationIngestClaimAuditIssue{UID: issueUID, RequireClaim: true})
	}
	if event.Type == "issue.created" || event.Type == "issue.snapshot" ||
		claimWorkMutationRequiresPeerClaim(event.Type) {
		references, err := payloadReferencedIssueUIDs(event, payload)
		if err != nil {
			return nil, err
		}
		for _, reference := range references {
			add(federationIngestClaimAuditIssue{UID: reference, RequireClaim: true})
		}
	}
	return output, nil
}

type claimWorkMutationInput struct {
	Project           db.Project
	Issue             db.Issue
	OffendingEventUID string
	EventType         string
	Actor             string
	HolderInstanceUID string
	RequireClaim      bool
}

func (s *Store) annotateClaimWorkMutationTx(
	ctx context.Context,
	tx *sql.Tx,
	input claimWorkMutationInput,
) ([]db.Event, error) {
	hub, err := enabledHubFederationBindingTx(ctx, tx, input.Project.ID)
	if err != nil || !hub {
		return nil, err
	}
	events, err := s.expireTimedClaimsTx(ctx, tx, input.Project.ID, time.Now().UTC(), 0)
	if err != nil {
		return nil, err
	}
	live, err := claimForIssueTx(ctx, tx, input.Issue.UID, true)
	if errors.Is(err, db.ErrNotFound) {
		return events, nil
	}
	if err != nil {
		return nil, err
	}
	if (input.RequireClaim || claimWorkMutationRequiresClaim(input.EventType)) &&
		!claimWorkCoveredByLiveClaim(live, input.HolderInstanceUID, input.Actor) {
		violation, err := s.insertClaimViolationEventTx(ctx, tx, input, live)
		if err != nil {
			return nil, err
		}
		events = append(events, violation)
	}
	if input.EventType == "issue.closed" {
		_, released, err := s.releaseClaimTx(ctx, tx, input.Project, input.Issue, live,
			"claim.released", input.Actor, "issue_closed", time.Now().UTC())
		if err != nil {
			return nil, err
		}
		events = append(events, released)
	}
	return events, nil
}

func (s *Store) insertClaimViolationEventTx(
	ctx context.Context,
	tx *sql.Tx,
	input claimWorkMutationInput,
	claim db.IssueClaim,
) (db.Event, error) {
	payload := map[string]any{
		"claim_uid": claim.ClaimUID, "holder": claim.Holder,
		"holder_instance_uid": claim.HolderInstanceUID, "client_kind": claim.ClientKind,
		"claim_kind": claim.ClaimKind, "purpose": claim.Purpose,
		"acquired_at": formatStoredTime(claim.AcquiredAt), "reason": "uncovered_work",
		"issue_uid": claim.IssueUID, "offending_event_uid": input.OffendingEventUID,
		"offending_event_type":          input.EventType,
		"offending_origin_instance_uid": input.HolderInstanceUID, "actor": input.Actor,
	}
	if claim.ExpiresAt != nil {
		payload["expires_at"] = formatStoredTime(*claim.ExpiresAt)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return db.Event{}, fmt.Errorf("marshal claim violation event: %w", err)
	}
	return s.insertEventTx(ctx, tx, eventInsert{
		ProjectID: input.Project.ID, ProjectUID: input.Project.UID,
		ProjectName: input.Project.Name, IssueID: &input.Issue.ID, IssueUID: &input.Issue.UID,
		Type: "claim.violated", Actor: input.Actor, Payload: string(body),
	})
}

func issueForClaimAuditInGroupTx(
	ctx context.Context,
	tx *sql.Tx,
	projectIDs []int64,
	issueUID string,
) (db.Issue, db.Project, error) {
	if len(projectIDs) == 0 {
		return db.Issue{}, db.Project{}, db.ErrNotFound
	}
	placeholders, args := postgresIDPlaceholders(projectIDs, 2)
	args = append([]any{issueUID}, args...)
	var issueID, projectID int64
	//nolint:gosec // IN values use generated placeholders with separately bound integer IDs.
	err := tx.QueryRowContext(ctx, `SELECT id,project_id FROM issues
WHERE uid=$1 AND project_id IN (`+placeholders+`)`, args...).Scan(&issueID, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Issue{}, db.Project{}, db.ErrNotFound
	}
	if err != nil {
		return db.Issue{}, db.Project{}, mapSQLError(err, nil)
	}
	issue, err := scanIssue(tx.QueryRowContext(ctx, issueSelect+` WHERE i.id=$1`, issueID))
	if err != nil {
		return db.Issue{}, db.Project{}, err
	}
	project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1`, projectID))
	return issue, project, err
}

func enabledHubFederationBindingTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
) (bool, error) {
	var role string
	var enabled int
	err := tx.QueryRowContext(ctx, `SELECT role,enabled FROM federation_bindings WHERE project_id=$1`,
		projectID).Scan(&role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapSQLError(err, nil)
	}
	return enabled == 1 && role == string(db.FederationRoleHub), nil
}

func claimWorkMutationRequiresClaim(eventType string) bool {
	switch eventType {
	case "issue.updated", "issue.assigned", "issue.unassigned",
		"issue.priority_set", "issue.priority_cleared", "issue.closed", "issue.reopened",
		"issue.soft_deleted", "issue.restored", "issue.labeled", "issue.unlabeled",
		"issue.linked", "issue.unlinked", "issue.links_changed", "issue.metadata_updated":
		return true
	default:
		return false
	}
}

func claimWorkMutationRequiresPeerClaim(eventType string) bool {
	return eventType == "issue.linked" || eventType == "issue.unlinked" ||
		eventType == "issue.links_changed"
}

func claimWorkCoveredByLiveClaim(claim db.IssueClaim, holderInstanceUID, actor string) bool {
	return claim.HolderInstanceUID == holderInstanceUID && claim.Holder == actor
}
