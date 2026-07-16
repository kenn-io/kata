package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// AdoptProjectIntoFederation adopts an existing local project into a hub
// federation and replaces its local event history with one portable baseline.
func (s *Store) AdoptProjectIntoFederation(
	ctx context.Context,
	params db.AdoptProjectIntoFederationParams,
) (db.AdoptProjectIntoFederationResult, error) {
	actor := strings.TrimSpace(params.Actor)
	if actor == "" {
		actor = "federation"
	}
	if !katauid.Valid(params.HubProjectUID) {
		return db.AdoptProjectIntoFederationResult{},
			fmt.Errorf("invalid hub project uid %q", params.HubProjectUID)
	}

	var output db.AdoptProjectIntoFederationResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.AdoptProjectIntoFederationResult{}
		project, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id=$1 FOR UPDATE`, params.ProjectID))
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return fmt.Errorf("adopt project into federation: project %d is archived", params.ProjectID)
		}
		if err := rejectIssueSyncedFederationProject(ctx, tx, params.ProjectID); err != nil {
			return err
		}

		existing, err := scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, params.ProjectID))
		if err == nil {
			if existing.Role == db.FederationRoleSpoke && existing.HubProjectUID == params.HubProjectUID {
				output = db.AdoptProjectIntoFederationResult{Project: project, Binding: existing}
				return nil
			}
			return fmt.Errorf("project %d already has %q federation binding", params.ProjectID, existing.Role)
		}
		if !errors.Is(err, db.ErrNotFound) {
			return err
		}

		issues, err := federationIssuesForSnapshot(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		pushFloor, err := federationAdoptionPushFloor(ctx, tx, project.ID, s.InstanceUID())
		if err != nil {
			return err
		}
		if err := lockEventSequenceTx(ctx, tx); err != nil {
			return err
		}
		boundary, err := nextEventHLCTx(ctx, tx, time.Now().UTC())
		if err != nil {
			return err
		}
		baselineCreatedAt := nowStoredTimestamp()

		if project.UID != params.HubProjectUID {
			if err := replaceProjectUIDTx(ctx, tx, project.ID, params.HubProjectUID); err != nil {
				return err
			}
			project.UID = params.HubProjectUID
		}
		if err := clearProjectClaimStateTx(ctx, tx, project.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, project.ID); err != nil {
			return fmt.Errorf("delete pre-adoption local events: %w", mapSQLError(err, nil))
		}

		pullCursor := params.ReplayHorizonEventID - 1
		if pullCursor < 0 {
			pullCursor = 0
		}
		allowInsecure := 0
		if params.AllowInsecure {
			allowInsecure = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_bindings(
project_id,role,hub_url,hub_project_id,hub_project_uid,
replay_horizon_event_id,pull_cursor_event_id,push_enabled,
push_cursor_event_id,bound_actor,allow_insecure,enabled
) VALUES($1,$2,$3,$4,$5,$6,$7,1,$8,$9,$10,1)`,
			project.ID, string(db.FederationRoleSpoke), params.HubURL, params.HubProjectID,
			params.HubProjectUID, params.ReplayHorizonEventID, pullCursor, pushFloor,
			actor, allowInsecure); err != nil {
			return fmt.Errorf("insert adoption federation binding: %w", mapSQLError(err, nil))
		}

		emitMetadataBaseline := len(project.Metadata) > 0 && string(project.Metadata) != "{}"
		if !emitMetadataBaseline && len(issues) == 0 {
			emitMetadataBaseline = true
		}
		if emitMetadataBaseline {
			payload, err := db.ProjectMetadataAdoptionPayload(project.Metadata)
			if err != nil {
				return err
			}
			if _, err := s.insertEventTx(ctx, tx, eventInsert{
				ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
				Type: "project.metadata_updated", Actor: actor, Payload: payload,
				HLC: &boundary, CreatedAt: baselineCreatedAt,
			}); err != nil {
				return err
			}
		}

		for _, issue := range issues {
			payload, err := federationIssueSnapshotPayload(ctx, tx, issue)
			if err != nil {
				return err
			}
			issueUID := issue.UID
			if _, err := s.insertEventTx(ctx, tx, eventInsert{
				ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
				IssueID: &issue.ID, IssueUID: &issueUID, Type: "issue.snapshot",
				Actor: actor, Payload: payload, HLC: &boundary, CreatedAt: baselineCreatedAt,
			}); err != nil {
				return err
			}
			output.AdoptionSnapshotCount++
		}

		binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1`, project.ID))
		if err != nil {
			return err
		}
		if err := reconcileFederationBindingTransitionLinks(ctx, tx, nil, binding); err != nil {
			return err
		}
		project, err = scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id=$1`, project.ID))
		if err != nil {
			return err
		}
		output.Project = project
		output.Binding = binding
		return nil
	})
	return output, err
}

func federationAdoptionPushFloor(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	originInstanceUID string,
) (int64, error) {
	var maxID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT MAX(id) FROM events
WHERE project_id=$1 AND origin_instance_uid=$2 AND `+pgFederationPushEventTypeCondition("type"),
		projectID, originInstanceUID).Scan(&maxID)
	if err != nil {
		return 0, fmt.Errorf("capture adoption push cursor floor: %w", mapSQLError(err, nil))
	}
	return maxID.Int64, nil
}

func replaceProjectUIDTx(ctx context.Context, tx *sql.Tx, projectID int64, projectUID string) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT rewrite_project_uid_for_adoption($1, $2)`, projectID, projectUID); err != nil {
		return fmt.Errorf("rewrite project uid for adoption: %w", mapSQLError(err, nil))
	}
	return nil
}

func clearProjectClaimStateTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	for _, statement := range []string{
		`DELETE FROM pending_claim_requests WHERE project_id=$1`,
		`DELETE FROM issue_claims WHERE project_id=$1`,
	} {
		if _, err := tx.ExecContext(ctx, statement, projectID); err != nil {
			return fmt.Errorf("clear project claim state: %w", mapSQLError(err, nil))
		}
	}
	return nil
}

func rejectIssueSyncedFederationProject(ctx context.Context, tx *sql.Tx, projectID int64) error {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT project_id FROM issue_sync_bindings
WHERE project_id=$1 AND enabled=1 FOR UPDATE`, projectID).Scan(&id)
	if err == nil {
		return db.ErrIssueSyncFederationBinding
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("check federation issue sync binding: %w", mapSQLError(err, nil))
}
