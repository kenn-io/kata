package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentsetup"
)

func agentSetupOptions(name string) (nativeAgentHookOptions, error) {
	workspace, err := agentHookWorkspacePath()
	if err != nil {
		return nativeAgentHookOptions{}, err
	}
	return agentSetupOptionsInWorkspace(name, workspace)
}

func agentSetupOptionsInWorkspace(name, workspace string) (nativeAgentHookOptions, error) {
	dir := workspace
	targets, err := selectNativeAgentHookTargets([]string{name}, false, "user", "", dir)
	if err != nil {
		return nativeAgentHookOptions{}, err
	}
	if len(targets) == 0 {
		return nativeAgentHookOptions{}, fmt.Errorf("no hook target for %s", name)
	}
	opts := targets[0].options
	opts.Executable, _, err = resolveAgentHookExecutable("", defaultAgentHookExecutableEnv())
	return opts, err
}

func setupHookRegistration(opts nativeAgentHookOptions, kind agentHookKind, workspace string) agenthook.InstallOptions {
	registration := kitAgentHookRegistration(opts, kind)
	registration.WorkspaceDir = workspace
	registration.Marker = "agent-hook contract " + opts.Agent
	if kind != contractHook {
		registration.Marker = "agent-hook attention-native " + opts.Agent + " " + string(kind)
	}
	return registration
}

func agentSetupItems(name, workspace string) ([]agentsetup.Item, error) {
	item := agentsetup.Item{Name: "hooks"}
	if name != "claude" && name != "codex" {
		item.Check = func(context.Context) (agentsetup.Status, error) {
			return agentsetup.Status{
				State: agentsetup.StateUnknown, Reason: "Hook inspection is unavailable for this agent",
				NextStep: "Inspect hooks in the agent",
			}, nil
		}
		return []agentsetup.Item{item}, nil
	}
	opts, err := agentSetupOptionsInWorkspace(name, workspace)
	if err != nil {
		return nil, err
	}
	kinds := []agentHookKind{contractHook, attentionStartHook, attentionEndHook}
	item.Install = func(ctx context.Context) error {
		for _, kind := range kinds {
			if err := ctx.Err(); err != nil {
				return err
			}
			registration := setupHookRegistration(opts, kind, workspace)
			conditional, err := agentSetupHasConditionalHook(agenthook.Agent(name), opts.ConfigPath, kind)
			if err != nil {
				return err
			}
			if conditional {
				if _, err := agenthook.Uninstall(agenthook.Agent(name), opts.ConfigPath, registration.Marker); err != nil {
					return err
				}
			}
			if _, err := agenthook.Install(agenthook.Agent(name), registration); err != nil {
				return err
			}
		}
		return nil
	}
	item.Check = func(ctx context.Context) (agentsetup.Status, error) {
		return checkAgentSetupWithOptions(ctx, name, workspace, opts, kinds)
	}
	return []agentsetup.Item{item}, nil
}

func checkAgentSetup(ctx context.Context, name string) (agentsetup.Status, error) {
	workspace, err := agentHookWorkspacePath()
	if err != nil {
		return agentsetup.Status{}, err
	}
	items, err := agentSetupItems(name, workspace)
	if err != nil {
		return agentsetup.Status{}, err
	}
	return items[0].Check(ctx)
}

func checkAgentSetupInWorkspace(ctx context.Context, name, workspace string) (agentsetup.Status, error) {
	if workspace == "" {
		return checkAgentSetup(ctx, name)
	}
	items, err := agentSetupItems(name, workspace)
	if err != nil {
		return agentsetup.Status{}, err
	}
	return items[0].Check(ctx)
}

func checkAgentSetupWithOptions(ctx context.Context, name, workspace string, opts nativeAgentHookOptions, kinds []agentHookKind) (agentsetup.Status, error) {
	status := agentsetup.Status{State: agentsetup.StateOK}
	missing := false
	var problem agentsetup.Status
	for _, kind := range kinds {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := agenthook.Check(probe, agenthook.Agent(name), setupHookRegistration(opts, kind, workspace))
		cancel()
		if err != nil {
			return agentsetup.Status{}, err
		}
		state := agentsetup.State(result.State)
		if state == agentsetup.StateUntrusted {
			if status.State == agentsetup.StateOK {
				status.State, status.Reason, status.NextStep = state, result.Reason, result.NextStep
			}
			status.HookCount += result.HookCount
			status.ConfigPath = result.ConfigPath
			continue
		}
		status.HookCount += result.HookCount
		if status.ConfigPath == "" {
			status.ConfigPath = result.ConfigPath
		}
		if state == agentsetup.StateMissing || state == agentsetup.StateOutdated {
			missing = true
			problem = agentsetup.Status{State: state, Reason: result.Reason, NextStep: "kata agent setup"}
			continue
		}
		if state != agentsetup.StateOK {
			return agentsetup.Status{
				State: state, Reason: result.Reason, NextStep: result.NextStep,
				ConfigPath: result.ConfigPath, HookCount: result.HookCount,
			}, nil
		}
	}
	if status.ConfigPath != "" && status.HookCount > 0 {
		entries, err := inspectAgentHookEntries(agenthook.Agent(name), status.ConfigPath)
		if err != nil {
			return agentsetup.Status{}, err
		}
		contract, start, end := kitAgentHookConfigured(agenthook.Agent(name), entries)
		if !contract || !start || !end {
			return agentsetup.Status{
				State: agentsetup.StateOutdated, Reason: "Kata hooks need updating",
				NextStep: "kata agent setup", ConfigPath: status.ConfigPath, HookCount: status.HookCount,
			}, nil
		}
	}
	if missing {
		if status.HookCount > 0 {
			problem.State, problem.Reason = agentsetup.StateOutdated, "Kata hooks need updating"
		}
		problem.ConfigPath, problem.HookCount = status.ConfigPath, status.HookCount
		return problem, nil
	}
	return status, nil
}

func agentSetupHasConditionalHook(agent agenthook.Agent, path string, kind agentHookKind) (bool, error) {
	entries, err := inspectAgentHookEntries(agent, path)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if classifyAgentHookHandlerFromEntry(entry) == kind && !sameAgentHookGroupBehavior(entry.GroupFields, nil) {
			return true, nil
		}
	}
	return false, nil
}

func newAgentSetupCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{Use: "setup [<harness>...]", Short: "Install and check agent hooks", RunE: func(cmd *cobra.Command, args []string) error {
		workspace, err := agentHookWorkspacePath()
		if err != nil {
			return err
		}
		report, err := agentsetup.Run(cmd.Context(), agentsetup.Options{
			Agents: args, CheckOnly: check, SetupCommand: "kata agent setup", RepairCommand: "kata agent setup",
			Items: func(name string) ([]agentsetup.Item, error) {
				return agentSetupItems(name, workspace)
			},
		})
		if err != nil {
			return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "or pass --agent", "or name an agent"))
		}
		for i, row := range report.Rows {
			if names, ok := strings.CutPrefix(row.NextStep, "kata agent setup --agent "); ok {
				report.Rows[i].NextStep = "kata agent setup " + strings.ReplaceAll(names, ",", " ")
			}
		}
		if currentOutputMode() == outputJSON {
			err = emitJSON(cmd.OutOrStdout(), report)
		} else {
			err = report.WriteText(cmd.OutOrStdout(), "kata agent setup --check")
		}
		if err != nil {
			return err
		}
		if report.ExitCode() != 0 {
			return &cliError{Kind: kindChecksFailed, ExitCode: 1, Message: "agent setup needs attention"}
		}
		return nil
	}}
	cmd.Flags().BoolVar(&check, "check", false, "Check readiness without installing hooks")
	return cmd
}

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Set up coding agents"}
	cmd.AddCommand(newAgentSetupCmd())
	return cmd
}
