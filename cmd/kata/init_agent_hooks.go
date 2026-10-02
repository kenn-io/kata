package main

import (
	"bytes"
	"context"
	"fmt"

	"go.kenn.io/kata/internal/config"
)

func planInitAgentHookSelection(names []string, start string, attention bool, api string) (nativeAgentHookPlan, []agentHookMutation, error) {
	var combined nativeAgentHookPlan
	if len(names) == 0 {
		return combined, nil, nil
	}
	discovered, err := config.DiscoverPaths(start)
	if err != nil {
		return combined, nil, err
	}
	dir := config.WriteDestination(discovered, start)
	targets, err := selectNativeAgentHookTargets(names, false, "project", "", dir)
	if err != nil {
		return combined, nil, err
	}
	results := make([]agentHookMutation, 0, len(targets))
	for _, target := range targets {
		result := agentHookMutation{Harness: target.capability.Name, State: "unchanged", Warnings: []string{}}
		opts := target.options
		opts.Executable = "kata"
		opts.Contract = target.capability.Contract
		opts.Attention = attention && (target.capability.AttentionStart || target.capability.AttentionEnd)
		if opts.Agent == "opencode" {
			opts.API = api
		}
		if opts.Agent == "muse" && opts.Attention {
			opts.Attention = false
			result.Reason = "Muse project contract configured; attention requires launcher tracking or authorized user managed attention"
			result.Warnings = append(result.Warnings, result.Reason)
		}
		plan, err := planNativeAgentHooks(opts, false)
		if err != nil {
			return combined, nil, err
		}
		combined.Changes = append(combined.Changes, plan.Changes...)
		result.ConfigPath = plan.Path
		result.Warnings = append(result.Warnings, plan.Warnings...)
		for _, change := range plan.Changes {
			result.Changed = result.Changed || (change.Remove && change.OriginalExists) || (!change.Remove && (!change.OriginalExists || !bytes.Equal(change.Original, change.Content)))
		}
		if result.Changed {
			result.State = "installed"
		}
		if result.Reason != "" {
			result.State = "partial"
		}
		if target.capability.Note != "" {
			result.Warnings = append(result.Warnings, target.capability.Name+": "+target.capability.Note)
		}
		results = append(results, result)
	}
	return combined, results, nil
}

// Retain runtime selection in the options before daemon contact. Artifact
// planners stay offline, including all planning repeated before publication.
func prepareInitHookOptions(ctx context.Context, opts callInitOpts) (callInitOpts, error) {
	modern := len(opts.AgentHooks) > 0
	if !modern && opts.ContractOnly {
		return opts, agentHookUsage("init --contract-only requires --agent-hooks")
	}
	if modern && (opts.WithHooks || opts.WithCodexHooks || len(opts.WithAgentHooks) > 0) {
		return opts, agentHookUsage("--agent-hooks cannot be combined with legacy --with-hooks, --with-codex-hooks or --with-agent-hooks")
	}
	names := opts.WithAgentHooks
	if modern {
		names = opts.AgentHooks
	}
	openCode := false
	for _, name := range names {
		capability, err := lookupAgentHookCapability(name)
		if err != nil {
			return opts, err
		}
		if modern && !capability.Project {
			return opts, agentHookUsage(capability.Name + " has no verified project hook discovery; use kata agent-hooks install in user scope")
		}
		if modern && !capability.Contract && opts.ContractOnly {
			return opts, agentHookUsage(capability.Name + " supports attention only; omit --contract-only")
		}
		openCode = openCode || capability.Name == "opencode"
	}
	if openCode && opts.OpenCodeAPI == "" {
		api, _, err := resolveOpenCodeRuntimeAPI(ctx, "")
		if err != nil {
			return opts, fmt.Errorf("init agent hooks: %w; alternatively initialize without hooks and run kata agent-hooks install opencode --local --api v1|v2", err)
		}
		opts.OpenCodeAPI = api
	}
	return opts, nil
}

func planInitHookOptions(opts callInitOpts, start string) (nativeAgentHookPlan, []agentHookMutation, error) {
	if len(opts.AgentHooks) == 0 {
		return planInitAgentHookSelection(opts.WithAgentHooks, start, true, opts.OpenCodeAPI)
	}
	return planInitAgentHookSelection(opts.AgentHooks, start, !opts.ContractOnly, opts.OpenCodeAPI)
}

func applyInitHookOptions(opts callInitOpts, dir string) (bool, []agentHookMutation, error) {
	plan, results, err := planInitHookOptions(opts, dir)
	if err != nil {
		return false, nil, err
	}
	changed, err := publishNativeAgentHookPlan(plan)
	for _, result := range results {
		emitHookWarnings(result.Warnings)
	}
	if err != nil {
		return changed, nil, fmt.Errorf("install project agent hooks: %w", err)
	}
	if len(opts.AgentHooks) == 0 {
		results = nil
	} // Preserve legacy init output.
	return changed, results, nil
}
