package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentsetup"
)

func agentSetupOptions(name string) (nativeAgentHookOptions, error) {
	dir, err := agentHookWorkspacePath()
	if err != nil {
		return nativeAgentHookOptions{}, err
	}
	targets, err := selectNativeAgentHookTargets([]string{name}, false, "user", "", dir)
	if err != nil {
		return nativeAgentHookOptions{}, err
	}
	opts := targets[0].options
	opts.Contract, opts.Attention = targets[0].capability.Contract, true
	opts.Executable, _, err = resolveAgentHookExecutable("", defaultAgentHookExecutableEnv())
	return opts, err
}

func agentSetupItem(name string, output io.Writer) agentsetup.Item {
	return agentsetup.Item{Name: "hooks", Install: func(ctx context.Context) error {
		cmd := newNativeAgentHooksMutationCmd(false)
		cmd.SetContext(ctx)
		cmd.SetOut(output)
		cmd.SetErr(output)
		cmd.SetIn(strings.NewReader(""))
		cmd.SetArgs([]string{name})
		return cmd.Execute()
	}, Check: func(ctx context.Context) (agentsetup.Status, error) {
		return checkAgentSetup(ctx, name)
	}}
}

func checkAgentSetup(ctx context.Context, name string) (agentsetup.Status, error) {
	opts, err := agentSetupOptions(name)
	if err != nil {
		return agentsetup.Status{}, err
	}
	plan, err := planNativeAgentHooks(opts, false)
	if errors.Is(err, errMuseAttentionPermission) || (err == nil && plan.CurrentContract && !plan.CurrentOwnedAttention && !plan.CurrentAttentionStart && !plan.CurrentAttentionEnd) {
		opts.Attention = false
		plan, err = planNativeAgentHooks(opts, false)
	}
	if err != nil {
		return agentsetup.Status{}, err
	}
	if name == "claude" || name == "codex" {
		installed, inspectErr := readAgentHookStatus(agenthook.Agent(name), plan.Path, opts.Dir)
		if inspectErr != nil {
			return agentsetup.Status{}, inspectErr
		}
		var executable string
		for _, entry := range installed.Entries {
			if !entry.ExecutableExists || !isKataHookExecutable(entry.Executable) || strings.ContainsAny(entry.Executable, "/\\") || (executable != "" && executable != entry.Executable) {
				executable = ""
				break
			}
			executable = entry.Executable
		}
		if executable != "" {
			opts.Executable = executable
			plan, err = planNativeAgentHooks(opts, false)
			if err != nil {
				return agentsetup.Status{}, err
			}
		}
	}
	status := agentsetup.Status{State: agentsetup.StateOK, ConfigPath: plan.Path}
	if nativeAgentHookPlanChanged(plan) {
		status.State, status.Reason, status.NextStep = agentsetup.StateMissing, "Kata hooks are not installed", "kata agent setup "+name
		if plan.CurrentContract || plan.CurrentAttentionStart || plan.CurrentAttentionEnd {
			status.State, status.Reason = agentsetup.StateOutdated, "Kata hooks need updating"
		}
		return status, nil
	}
	if name != "claude" && name != "codex" {
		status.State, status.Reason, status.NextStep = agentsetup.StateUnknown, "Hook inspection is unavailable for this agent", "Inspect hooks in the agent"
		return status, nil
	}
	counts := map[agentsetup.State]int{}
	kinds := []agentHookKind{contractHook}
	if opts.Attention {
		kinds = append(kinds, attentionStartHook, attentionEndHook)
	}
	for _, kind := range kinds {
		registration := kitAgentHookRegistration(opts, kind)
		command, err := agenthook.BuildCommand(registration.Executable, registration.Arguments...)
		if err != nil {
			return status, err
		}
		registration.Marker = command.Native
		if runtime.GOOS == "windows" && name == "claude" {
			registration.Marker = strings.Join(append([]string{registration.Executable}, registration.Arguments...), " ")
		}
		// Each registration starts its own native check, so each gets its own bound.
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := agenthook.Check(probe, agenthook.Agent(name), registration)
		cancel()
		if err != nil {
			return status, err
		}
		state := agentsetup.State(result.State)
		counts[state] += result.HookCount
		if state == agentsetup.StateMissing || state == agentsetup.StateOutdated || state == agentsetup.StateUnknown || state == agentsetup.StateDisabled {
			status.State, status.Reason, status.NextStep = state, result.Reason, result.NextStep
			return status, nil
		}
		if state != agentsetup.StateOK && (status.State == agentsetup.StateOK || status.State == agentsetup.StateUntrusted) {
			status.State, status.Reason, status.NextStep = state, result.Reason, result.NextStep
		}
	}
	status.HookCount = counts[status.State]
	return status, nil
}

// agentSetupFallback runs the item for Kata-only profiles, which Kit's runner cannot parse.
func agentSetupFallback(ctx context.Context, name string, checkOnly bool, item agentsetup.Item) (agentsetup.Row, error) {
	row := agentsetup.Row{Agent: name, AgentName: name, Piece: item.Name}
	if err := ctx.Err(); err != nil {
		return row, err
	}
	var err error
	if !checkOnly {
		err = item.Install(ctx)
	}
	if err == nil {
		row.Status, err = item.Check(ctx)
	}
	if err != nil {
		row.Status = agentsetup.Status{State: agentsetup.StateFailed, Reason: err.Error(), NextStep: "kata agent setup " + name}
	}
	return row, ctx.Err()
}

func newAgentSetupCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{Use: "setup [<harness>...]", Short: "Install and check agent hooks", RunE: func(cmd *cobra.Command, args []string) error {
		output := io.Writer(io.Discard)
		if currentOutputMode() == outputHuman {
			output = cmd.ErrOrStderr()
		}
		items := func(name string) ([]agentsetup.Item, error) {
			return []agentsetup.Item{agentSetupItem(name, output)}, nil
		}
		var kitNames, otherNames []string
		for _, name := range args {
			capability, err := lookupAgentHookCapability(name)
			if err != nil {
				return err
			}
			name = capability.Name
			if _, err := agenthook.ParseAgent(name); err == nil {
				kitNames = append(kitNames, name)
			} else if !slices.Contains(otherNames, name) {
				otherNames = append(otherNames, name)
			}
		}
		var report agentsetup.Report
		var err error
		if len(kitNames) > 0 || len(args) == 0 {
			report, err = agentsetup.Run(cmd.Context(), agentsetup.Options{Agents: kitNames, CheckOnly: check, SetupCommand: "kata agent setup", Items: items})
		}
		for _, name := range otherNames {
			if err != nil {
				break
			}
			var row agentsetup.Row
			row, err = agentSetupFallback(cmd.Context(), name, check, agentSetupItem(name, output))
			report.Rows = append(report.Rows, row)
		}
		if err != nil {
			return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "or pass --agent", "or name an agent"))
		}
		// Kata's global --agent selects output mode, so setup takes positional names.
		for i, row := range report.Rows {
			if names, ok := strings.CutPrefix(row.NextStep, "kata agent setup --agent "); ok {
				report.Rows[i].NextStep = "kata agent setup " + strings.ReplaceAll(names, ",", " ")
			}
		}
		switch currentOutputMode() {
		case outputJSON:
			err = emitJSON(cmd.OutOrStdout(), report)
		default:
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
