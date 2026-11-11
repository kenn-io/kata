package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/cron"
)

// CronDefinitionSnapshots captures portable definitions and run history
// in the federation boundary transaction without execution coordination.
func CronDefinitionSnapshots(ctx context.Context, tx *sql.Tx, project Project) ([]CronEvent, error) {
	result := []CronEvent{}
	query := CronSQL{Query: tx}
	jobs, err := query.Jobs(ctx, CronList{ProjectID: project.ID, IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	workflows, err := query.workflows(ctx, CronList{ProjectID: project.ID, IncludeDeleted: true}, true)
	if err != nil {
		return nil, err
	}
	add := func(kind string, value CronDefinition, definition any) error {
		raw, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(CronDefinitionEvent{UID: value.UID, ProjectUID: project.UID, Name: value.Name, Definition: raw, Author: value.Author, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, DeletedAt: value.DeletedAt, DefinitionEventUID: value.DefinitionEventUID, DefinitionHLC: &value.DefinitionHLC})
		if err != nil {
			return err
		}
		result = append(result, CronEvent{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, Type: "cron." + kind + ".snapshot", Payload: string(payload)})
		return nil
	}
	for _, job := range jobs {
		if err := add("job", job.CronDefinition, job.Definition); err != nil {
			return nil, err
		}
	}
	for _, workflow := range workflows {
		if err := add("workflow", workflow.CronDefinition, workflow.Definition); err != nil {
			return nil, err
		}
	}
	runs, err := cronRunSnapshots(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	return append(result, runs...), nil
}

// CronMaterializationNeeded reports whether a federated rebuild must rewrite
// cron rows. Those rows derive only from cron events, so an ingest batch that
// accepted none leaves them unchanged; nil accepted UIDs mean a full rebuild.
func CronMaterializationNeeded(events []FoldEvent, acceptedEventUIDs []string) bool {
	if acceptedEventUIDs == nil {
		return true
	}
	accepted := make(map[string]bool, len(acceptedEventUIDs))
	for _, uid := range acceptedEventUIDs {
		accepted[uid] = true
	}
	for _, event := range events {
		if accepted[event.UID] && EventRequiredFeatures(event.Type) == CronEventFeature {
			return true
		}
	}
	return false
}

// MaterializeCronDefinitions writes the winning shared definitions and
// independent observations. Reordered references do not start execution.
func MaterializeCronDefinitions(ctx context.Context, tx *sql.Tx, projectID int64, projectUID string, p FoldProjection, validator *CronReplayValidator) error {
	if err := p.cronBlockedProjects[projectUID]; err != nil {
		return fmt.Errorf("%w: unsupported retained workflow: %v", ErrFederationIngestValidation, err)
	}
	put := func(job bool, value CronDefinition, definition any, owner string) error {
		if owner != projectUID {
			return fmt.Errorf("%w: cron project identity mismatch", ErrFederationIngestValidation)
		}
		table := cronTable(job)
		var existingProject int64
		err := tx.QueryRowContext(ctx, "SELECT project_id FROM "+table+" WHERE uid=$1", value.UID).Scan(&existingProject)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && existingProject != projectID {
			return fmt.Errorf("%w: cron UID belongs to another project", ErrFederationIngestValidation)
		}
		repair := false
		if err == nil && !job {
			var raw, clockJSON, eventUID, author string
			if err := tx.QueryRowContext(ctx, `SELECT definition_json,definition_hlc_json,definition_event_uid,author FROM cron_workflows WHERE uid=$1`, value.UID).Scan(&raw, &clockJSON, &eventUID, &author); err != nil {
				return err
			}
			var clock CronDefinitionHLC
			if err := json.Unmarshal([]byte(clockJSON), &clock); err != nil {
				return err
			}
			current := CronDefinition{DefinitionHLC: clock, DefinitionEventUID: eventUID}
			order := compareClock(cronDefinitionClock(value), cronDefinitionClock(current))
			_, currentErr := cron.DecodeWorkflow([]byte(raw))
			incomingRaw, held := definition.(jsontext.Value)
			if currentErr != nil && order <= 0 {
				return nil
			}
			if held {
				if order < 0 {
					return nil
				}
				// Equal clocks alone do not prove that two versions are the same.
				if order == 0 && eventUID == value.DefinitionEventUID && author == value.Author && currentErr == nil {
					_, incomingErr := cron.DecodeWorkflow(incomingRaw)
					repair = incomingErr != nil
				}
			}
		}
		document, err := cronDefinitionJSON(definition)
		if err != nil {
			return err
		}
		clock, err := json.Marshal(value.DefinitionHLC)
		if err != nil {
			return err
		}
		//nolint:gosec // Table identifier is selected from the two fixed definition tables; values are bound.
		_, err = tx.ExecContext(ctx, "INSERT INTO "+table+`(uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at,deleted_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,1,$8,$9,$10)
 ON CONFLICT(uid) DO UPDATE SET name=excluded.name,definition_json=excluded.definition_json,definition_event_uid=excluded.definition_event_uid,definition_hlc_json=excluded.definition_hlc_json,author=excluded.author,revision=`+table+`.revision+1,created_at=excluded.created_at,updated_at=excluded.updated_at,deleted_at=excluded.deleted_at WHERE `+table+`.definition_event_uid<>excluded.definition_event_uid OR `+table+`.author<>excluded.author OR $11`, value.UID, projectID, value.Name, string(document), value.DefinitionEventUID, string(clock), value.Author, value.CreatedAt.UTC().Format(EventTimestampFormat), value.UpdatedAt.UTC().Format(EventTimestampFormat), cronTimeValue(value.DeletedAt), repair)
		return err
	}
	for _, job := range p.CronJobs {
		if err := put(true, job.CronDefinition, job.Definition, job.ProjectUID); err != nil {
			return err
		}
	}
	for _, workflow := range p.CronWorkflows {
		var definition any = workflow.Definition
		if len(workflow.RawDefinition) != 0 {
			definition = workflow.RawDefinition
		}
		if err := put(false, workflow.CronDefinition, definition, workflow.ProjectUID); err != nil {
			return err
		}
	}
	if err := materializeCronRuns(ctx, tx, projectID, projectUID, p, validator); err != nil {
		return err
	}
	return nil
}

// PrepareCronAdoption captures portable history before rebinding its
// project envelope; ordinary federation guards remain with the caller.
func PrepareCronAdoption(ctx context.Context, tx *sql.Tx, project Project, targetUID string, emptyOnly bool) ([]CronEvent, error) {
	if err := RequireLocalProjectWorkflows(ctx, tx, project.UID); err != nil {
		return nil, err
	}
	// Until adoption carries the target handshake into this transaction, do not
	// replace argv history that the target may be unable to accept.
	required, err := ProjectWorkflowRequiredFeatures(ctx, tx, project.UID)
	if err != nil {
		return nil, err
	}
	if err := RequireEventFeatures("", required); err != nil {
		return nil, err
	}
	// Adoption replaces history with new spoke-originated snapshots. Native
	// delivery state cannot enter through that path; refuse before rebinding or
	// deleting local history so the current authority can keep delivering.
	native, err := projectHasNativeStateForAdoption(ctx, tx, project.UID)
	if err != nil {
		return nil, err
	}
	if native {
		return nil, fmt.Errorf("%w: native recurring delivery history must remain on its owning hub", ErrFederatedSpokeUnsupported)
	}
	events, err := CronDefinitionSnapshots(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	if emptyOnly && len(events) > 0 {
		return nil, ErrFederationProjectNotEmpty
	}
	projectUID, err := json.Marshal(targetUID)
	if err != nil {
		return nil, err
	}
	for i := range events {
		payload := map[string]jsontext.Value{}
		if err := json.Unmarshal([]byte(events[i].Payload), &payload); err != nil {
			return nil, err
		}
		if payload == nil {
			return nil, fmt.Errorf("%w: adoption payload must be an object", ErrFederationIngestValidation)
		}
		payload["project_uid"] = projectUID
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		events[i].Payload = string(raw)
		events[i].ProjectUID = targetUID
	}
	return events, nil
}
