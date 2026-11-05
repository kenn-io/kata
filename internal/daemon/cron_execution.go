package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func cronExecutionPolicy(ctx context.Context, cfg ServerConfig, project db.Project) (api.CronExecutionPolicy, error) {
	policy := api.CronExecutionPolicy{Mode: string(config.ModeAll), Source: "local"}
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
		policy.Mode = string(config.ModeNone)
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
	if !policy.Replica || policy.Mode == string(config.ModeAll) {
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
	var workflows []string
	if in.Body.JobUID != nil {
		id := *in.Body.JobUID
		job, readErr := cfg.DB.CronJob(ctx, project.ID, id)
		if errors.Is(readErr, db.ErrNotFound) {
			return cronExecutionMissingDefinition(policy, "job", id)
		}
		if readErr != nil {
			return readErr
		}
		// Only the current notify action is exempt. Adapters refresh current
		// definitions before launch; an older reference counts as execution.
		notify := in.Body.DefinitionEventUID != nil &&
			strings.EqualFold(*in.Body.DefinitionEventUID, job.DefinitionEventUID) &&
			job.Definition.Action.Kind == "notify"
		if !notify {
			if !executionEligible(config.CronExecuteMode(policy.Mode), policy.MemberActor, job.Author) {
				return cronExecutionRefusal(policy, "job", id)
			}
			if job.Definition.Action.WorkflowUID != "" {
				workflows = append(workflows, job.Definition.Action.WorkflowUID)
			}
		}
	}
	if in.Body.WorkflowUID != nil && !slices.Contains(workflows, *in.Body.WorkflowUID) {
		workflows = append(workflows, *in.Body.WorkflowUID)
	}
	for _, id := range workflows {
		workflow, readErr := cfg.DB.CronWorkflow(ctx, project.ID, id)
		if errors.Is(readErr, db.ErrNotFound) {
			return cronExecutionMissingDefinition(policy, "workflow", id)
		}
		if readErr != nil {
			return readErr
		}
		if !executionEligible(config.CronExecuteMode(policy.Mode), policy.MemberActor, workflow.Author) {
			return cronExecutionRefusal(policy, "workflow", id)
		}
	}
	// Policy uses check-time binding and authorship.
	// This cooperative admission check is not a transaction authorization fence.
	return nil
}

// executionEligible compares exact upstream member identities.
func executionEligible(mode config.CronExecuteMode, memberActor, author string) bool {
	switch mode {
	case config.ModeOwned:
		return memberActor == author
	default:
		return false
	}
}

func cronExecutionRefusal(policy api.CronExecutionPolicy, kind, id string) error {
	return api.NewError(403, "cron_execution_not_enabled", fmt.Sprintf("this daemon has not opted in to executing %s %s for this replicated project", kind, id), "Set [cron].replica_execute or [[cron.project]] execute in config.toml on this daemon, or run it on the owning hub.", map[string]any{"mode": policy.Mode, "member_actor": policy.MemberActor})
}

func cronExecutionMissingDefinition(policy api.CronExecutionPolicy, kind, id string) error {
	return api.NewError(403, "cron_execution_not_enabled", fmt.Sprintf("%s %s definition was not found on this daemon for this replicated project", kind, id), "Check the definition UID and sync this replica before retrying.", map[string]any{"mode": policy.Mode, "member_actor": policy.MemberActor})
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
