package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

var errUnsupportedCronJobVersion = errors.New("unsupported cron job version")

func cronExecutionPolicy(ctx context.Context, cfg ServerConfig, project db.Project) (api.CronExecutionPolicy, error) {
	policy := api.CronExecutionPolicy{Mode: string(cron.ModeAll), Source: "local"}
	binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
	if errors.Is(err, db.ErrNotFound) {
		return policy, nil
	}
	if err != nil {
		return policy, err
	}
	if binding.Role != db.FederationRoleSpoke {
		return policy, nil
	}
	policy = api.CronExecutionPolicy{Replica: true, Mode: cfg.CronExecution.ReplicaExecute, MemberActor: binding.Actor, Source: "replica_default"}
	if policy.Mode == "" {
		policy.Mode = string(cron.ModeNone)
	}
	for _, override := range cfg.CronExecution.Projects {
		if override.Name == project.Name {
			policy.Mode = override.Execute
			policy.Source = "project"
			break
		}
	}
	return policy, nil
}

func checkCronExecution(ctx context.Context, cfg ServerConfig, project db.Project, in *api.ObserveCronRunRequest) error {
	policy, err := cronExecutionPolicy(ctx, cfg, project)
	if err != nil {
		return err
	}
	if !policy.Replica || policy.Mode == string(cron.ModeAll) {
		return nil
	}
	if _, err = cfg.DB.CronRun(ctx, project.ID, in.RunUID); err == nil {
		// Existing observations may finish after opt-out, but the store still
		// checks their immutable identity, attribution and revision.
		return nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return err
	}
	kind, id, author := "workflow", "", ""
	if in.Body.JobUID != nil {
		kind, id = "job", *in.Body.JobUID
		job, readErr := cfg.DB.CronJob(ctx, project.ID, id)
		if errors.Is(readErr, db.ErrNotFound) {
			return cronExecutionRefusal(policy, kind, id)
		}
		if readErr != nil {
			return readErr
		}
		executes, readErr := cronObservationExecutes(ctx, cfg.DB, project, job, in.Body.DefinitionEventUID)
		if errors.Is(readErr, errUnsupportedCronJobVersion) {
			return cronExecutionRefusal(policy, kind, id)
		}
		if readErr != nil {
			return readErr
		}
		switch {
		case executes:
			author = job.Author
		case in.Body.WorkflowUID == nil:
			// Notify evidence does not activate adapter execution.
			return nil
		default:
			kind, id = "workflow", *in.Body.WorkflowUID
		}
	}
	if kind == "workflow" {
		// Let storage validate observations without any reference pair.
		if in.Body.WorkflowUID == nil {
			return nil
		}
		id = *in.Body.WorkflowUID
		workflow, readErr := cfg.DB.CronWorkflow(ctx, project.ID, id)
		if errors.Is(readErr, db.ErrNotFound) {
			return cronExecutionRefusal(policy, kind, id)
		}
		if readErr != nil {
			return readErr
		}
		author = workflow.Author
	}
	// Policy uses check-time binding and authorship. Adoption can change authors;
	// this cooperative admission check is not a transaction authorization fence.
	if !cron.ExecutionEligible(cron.ExecuteMode(policy.Mode), policy.MemberActor, author) {
		return cronExecutionRefusal(policy, kind, id)
	}
	return nil
}

func cronExecutionRefusal(policy api.CronExecutionPolicy, kind, id string) error {
	return api.NewError(403, "cron_execution_not_enabled", fmt.Sprintf("this daemon has not opted in to executing %s %s for this replicated project", kind, id), "Set [[cron.project]] execute in config.toml on this daemon, or run it on the owning hub.", map[string]any{"mode": policy.Mode, "member_actor": policy.MemberActor})
}

func cronJobActionExecutes(definition cron.JobDefinition) (bool, error) {
	if definition.Version != 1 {
		return false, errUnsupportedCronJobVersion
	}
	return definition.Action.Kind != "notify", nil
}

// CronExecutionWarnings describes unmatched or ineffective overrides once at
// startup. Unknown names are allowed because a replica may be joined later.
func CronExecutionWarnings(ctx context.Context, store db.Storage, cfg config.CronConfig) ([]string, error) {
	if len(cfg.Projects) == 0 {
		return nil, nil
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]db.Project, len(projects))
	for _, project := range projects {
		byName[project.Name] = project
	}
	var warnings []string
	for _, override := range cfg.Projects {
		project, ok := byName[override.Name]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("cron.project %q matches no current project", override.Name))
			continue
		}
		binding, err := store.FederationBindingByProject(ctx, project.ID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
		if errors.Is(err, db.ErrNotFound) || binding.Role != db.FederationRoleSpoke {
			warnings = append(warnings, fmt.Sprintf("cron.project %q is not a replica; execution remains all", override.Name))
		}
	}
	return warnings, nil
}

// Historical observations retain their action kind after definition edits.
// Compacted history can retain reference identity without the old document;
// that is insufficient to establish the notify exemption, so deny it safely.
func cronObservationExecutes(ctx context.Context, store db.Storage, project db.Project, job db.CronJob, eventUID *string) (bool, error) {
	if eventUID == nil {
		return true, nil
	}
	if strings.EqualFold(*eventUID, job.DefinitionEventUID) {
		return cronJobActionExecutes(job.Definition)
	}
	events, err := store.EventsByUIDs(ctx, project.ID, []string{strings.ToUpper(*eventUID)})
	if errors.Is(err, db.ErrNotFound) {
		executes, found, readErr := cronSnapshotActionExecutes(ctx, store, project, job, *eventUID)
		if readErr != nil {
			return true, readErr
		}
		if !found {
			return true, nil
		}
		return executes, nil
	}
	if err != nil {
		return true, err
	}
	for _, event := range events {
		if !strings.HasPrefix(event.Type, "cron.job.") {
			continue
		}
		var payload db.CronDefinitionEvent
		if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil {
			return true, err
		}
		if !strings.EqualFold(payload.UID, job.UID) || payload.ProjectUID != project.UID {
			continue
		}
		definition, err := cron.DecodeJob(payload.Definition)
		if err != nil {
			return true, err
		}
		return cronJobActionExecutes(definition)
	}
	return true, nil
}

// cronSnapshotActionExecutes follows a definition version through its retained
// snapshot payload. Snapshot envelope UIDs differ from the referenced definition
// event UID, so the store filters on both payload identities in the query.
func cronSnapshotActionExecutes(ctx context.Context, store db.Storage, project db.Project, job db.CronJob, definitionEventUID string) (bool, bool, error) {
	raw, err := store.CronJobSnapshotPayload(ctx, project.ID, job.UID, definitionEventUID)
	if errors.Is(err, db.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var payload db.CronDefinitionEvent
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return false, false, err
	}
	if payload.ProjectUID != project.UID {
		return false, false, nil
	}
	definition, err := cron.DecodeJob(payload.Definition)
	if err != nil {
		return false, false, err
	}
	executes, err := cronJobActionExecutes(definition)
	return executes, true, err
}
