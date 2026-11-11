package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentsetup"
)

var errAgentSetupExecutables = errors.New("kata hooks use different executables")

func agentSetupOptions(name string) (nativeAgentHookOptions, string, error) {
	dir, err := agentHookWorkspacePath()
	if err != nil {
		return nativeAgentHookOptions{}, "", err
	}
	targets, err := selectNativeAgentHookTargets([]string{name}, false, "user", "", dir)
	if err != nil {
		return nativeAgentHookOptions{}, "", err
	}
	target := targets[0]
	opts := target.options
	var exe, warning string
	if agentHookUsesKit(name) {
		status, err := readAgentHookStatus(agenthook.Agent(name), opts.ConfigPath, dir)
		if err != nil {
			return opts, "", err
		}
		for _, entry := range status.Entries {
			if entry.Executable == "" || (exe != "" && exe != entry.Executable) {
				return opts, "", errAgentSetupExecutables
			}
			exe = entry.Executable
		}
	} else {
		opts.Contract, opts.Attention = target.capability.Contract, true
		opts.Executable = "kata"
		plan, err := planAgentSetup(&opts)
		if err != nil {
			return opts, "", err
		}
		for _, change := range plan.Changes {
			if !change.OriginalExists {
				continue
			}
			var executable string
			switch name {
			case "pi":
				if meta, err := parsePiAgentHookMetadata(change.Original); err == nil {
					executable = meta.Executable
				}
			case "opencode", "amp":
				if meta, err := parsePluginAgentHookMetadata(change.Original); err == nil {
					executable = meta.Executable
				}
			case "openclaw":
				line, _, _ := bytes.Cut(change.Original, []byte("\n"))
				if metadata, ok := bytes.CutPrefix(line, []byte(openClawManagedPrefix)); ok {
					var meta openClawManaged
					if json.Unmarshal(metadata, &meta) == nil {
						executable = meta.Executable
					}
				}
			default:
				values, err := agentSetupExtraExecutables(name, change.Original)
				if err != nil {
					return opts, "", err
				}
				for _, value := range values {
					if executable != "" && executable != value {
						return opts, "", errAgentSetupExecutables
					}
					executable = value
				}
			}
			if executable != "" && exe != "" && executable != exe {
				return opts, "", errAgentSetupExecutables
			}
			if executable != "" {
				exe = executable
			}
		}
	}
	if exe == "" {
		exe, warning, err = resolveAgentHookExecutable("", defaultAgentHookExecutableEnv())
	}
	if err != nil {
		return nativeAgentHookOptions{}, "", err
	}
	opts.Executable, opts.Contract, opts.Attention = exe, target.capability.Contract, true
	return opts, warning, nil
}

func agentSetupExtraExecutables(name string, data []byte) ([]string, error) {
	var handlers []map[string]any
	if name == "kimi" || name == "kimi-code" {
		blocks, err := parseExtraTOMLHooks(data)
		if err != nil {
			return nil, err
		}
		for _, block := range blocks {
			handlers = append(handlers, block.fields)
		}
	} else {
		root, err := parseExtraJSON(data, true)
		if err != nil {
			return nil, err
		}
		sections := map[string]map[string]any{}
		if name == "antigravity" {
			for key, value := range root {
				if section, ok := value.(map[string]any); ok {
					sections[key] = section
				}
			}
		} else {
			section := root
			if hooks, ok := root["hooks"].(map[string]any); ok {
				section = hooks
			}
			if name == "zcode" {
				section, _ = section["events"].(map[string]any)
			}
			sections[""] = section
		}
		registrations, err := extraJSONRegistrations(name, sections)
		if err != nil {
			return nil, err
		}
		for _, registration := range registrations {
			handlers = append(handlers, registration.handler)
		}
	}
	var values []string
	for _, handler := range handlers {
		if extraHookKind(name, handler) == "" {
			continue
		}
		command, _ := handler["command"].(string)
		if runtime.GOOS == "windows" {
			for _, key := range []string{"commandWindows", "command_windows"} {
				if alternate, ok := handler[key].(string); ok && alternate != "" {
					command = alternate
					break
				}
			}
		}
		if name == "zcode" {
			values = append(values, command)
			continue
		}
		value, err := agentHookCommandExecutable(command, runtime.GOOS, false)
		if err != nil {
			return nil, errAgentSetupExecutables
		}
		values = append(values, value)
	}
	return values, nil
}

func agentSetupRecovery(name string) string {
	exe, _, err := resolveAgentHookExecutable("", defaultAgentHookExecutableEnv())
	if err != nil {
		return "Install Kata on PATH, then run kata agent-hook install " + name + " --executable kata"
	}
	command, _ := agenthook.BuildCommand(exe, "agent-hook", "install", name, "--executable", exe)
	return command.Native
}

func agentSetupItem(name string, warn func(string)) ([]agentsetup.Item, error) {
	var opts nativeAgentHookOptions
	var warning string
	var initErr error
	initialized := false
	initialize := func() error {
		if !initialized {
			opts, warning, initErr = agentSetupOptions(name)
			initialized = true
		}
		return initErr
	}
	return []agentsetup.Item{{Name: "hooks", Install: func(ctx context.Context) error {
		if err := initialize(); err != nil {
			if errors.Is(err, errAgentSetupExecutables) {
				return nil
			}
			return err
		}
		plan, err := planAgentSetup(&opts)
		if err != nil {
			return err
		}
		if opts.Agent == "opencode" {
			opts.API, _, err = resolveOpenCodeRuntimeAPI(ctx, plan.CurrentAPI)
			if err != nil {
				return err
			}
			plan, err = planNativeAgentHooks(opts, false)
			if err != nil {
				return err
			}
		}
		if _, err := agenthook.ParseAgent(name); err != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		changed, err := publishNativeAgentHookPlan(plan)
		if err == nil && changed && warning != "" && warn != nil {
			warning = strings.Replace(warning, " (use --executable for a stable path)", ". Install Kata on PATH and rerun kata agent setup "+name, 1)
			warn(warning)
		}
		return err
	}, Check: func(ctx context.Context) (agentsetup.Status, error) {
		if err := initialize(); err != nil {
			if errors.Is(err, errAgentSetupExecutables) {
				return agentsetup.Status{State: agentsetup.StateUnknown, Reason: err.Error(), NextStep: agentSetupRecovery(name)}, nil
			}
			return agentsetup.Status{}, err
		}
		plan, err := planAgentSetup(&opts)
		if err != nil {
			return agentsetup.Status{}, err
		}
		for _, message := range plan.Warnings {
			warn(message)
		}
		if capability, err := lookupAgentHookCapability(name); err == nil && capability.Note != "" {
			warn(capability.Note)
		}
		return checkAgentSetup(ctx, opts, plan)
	}}}, nil
}

func checkAgentSetup(ctx context.Context, opts nativeAgentHookOptions, plan nativeAgentHookPlan) (agentsetup.Status, error) {
	status := agentsetup.Status{State: agentsetup.StateOK, ConfigPath: opts.ConfigPath}
	if opts.Agent == "claude" || opts.Agent == "codex" {
		if _, err := exec.LookPath(opts.Agent); err != nil {
			configured, err := readAgentHookStatus(agenthook.Agent(opts.Agent), opts.ConfigPath, opts.Dir)
			if err != nil {
				return status, err
			}
			if len(configured.Entries) > 0 {
				return agentsetup.Status{State: agentsetup.StateUnknown, ConfigPath: opts.ConfigPath, Reason: "Agent executable is missing", NextStep: "Install " + opts.Agent + " or add it to PATH"}, nil
			}
		}
	}
	status.ConfigPath = plan.Path
	for _, change := range plan.Changes {
		if (change.Remove && change.OriginalExists) || (!change.Remove && (!change.OriginalExists || !bytes.Equal(change.Original, change.Content))) {
			status.State, status.Reason = agentsetup.StateMissing, "Kata hooks are not installed"
			if agentHookUsesKit(opts.Agent) {
				entries, err := parseAgentHookEntries(agenthook.Agent(opts.Agent), change.Original)
				if err != nil {
					return status, err
				}
				for _, entry := range entries {
					if classifyAgentHookHandlerFromEntry(entry) != "" {
						status.State, status.Reason = agentsetup.StateOutdated, "Kata hooks need updating"
						break
					}
				}
			} else if plan.CurrentContract || plan.CurrentAttentionStart || plan.CurrentAttentionEnd {
				status.State, status.Reason = agentsetup.StateOutdated, "Kata hooks need updating"
			}
			status.NextStep = "kata agent setup " + opts.Agent
			return status, nil
		}
	}
	if opts.Agent != "claude" && opts.Agent != "codex" {
		if opts.Agent == "muse" && !opts.Attention {
			return agentsetup.Status{State: agentsetup.StateUnknown, ConfigPath: plan.Path, Reason: "Muse attention hooks need permission", NextStep: "kata agent-hook install muse --managed-attention"}, nil
		}
		return agentsetup.Status{State: agentsetup.StateUnknown, ConfigPath: plan.Path, Reason: "Hook inspection is unavailable for this agent", NextStep: "Inspect hooks in the agent"}, nil
	}
	entries, err := inspectAgentHookEntries(agenthook.Agent(opts.Agent), opts.ConfigPath)
	if err != nil {
		return status, err
	}
	counts := map[agentsetup.State]int{}
	for _, kind := range []agentHookKind{contractHook, attentionStartHook, attentionEndHook} {
		registration := kitAgentHookRegistration(opts, kind)
		// Kit matches substrings; use the full owned command and refuse an
		// ambiguous match before asking it for native trust or policy state.
		registration.Marker = ""
		for _, entry := range entries {
			if classifyAgentHookHandlerFromEntry(entry) == kind {
				registration.Marker = agentSetupCheckCommand(entry)
				break
			}
		}
		if registration.Marker == "" {
			return agentsetup.Status{State: agentsetup.StateUnknown, ConfigPath: opts.ConfigPath, Reason: "Kata hook configuration changed during inspection", NextStep: "Run kata agent setup --check again"}, nil
		}
		for _, entry := range entries {
			if classifyAgentHookHandlerFromEntry(entry) != kind && strings.Contains(agentSetupCheckCommand(entry), registration.Marker) {
				return agentsetup.Status{State: agentsetup.StateUnknown, ConfigPath: opts.ConfigPath, Reason: "A foreign hook contains the Kata command", NextStep: "Inspect hooks in the agent"}, nil
			}
		}
		result, err := agenthook.Check(ctx, agenthook.Agent(opts.Agent), registration)
		if err != nil {
			return status, err
		}
		state := agentsetup.State(result.State)
		counts[state] += result.HookCount
		if state == agentsetup.StateMissing || state == agentsetup.StateOutdated {
			return agentsetup.Status{State: agentsetup.StateUnknown, ConfigPath: opts.ConfigPath, Reason: "Hook inspection found conflicting commands", NextStep: "Inspect hooks in the agent"}, nil
		}
		if state != agentsetup.StateOK && (status.State == agentsetup.StateOK || status.State == agentsetup.StateUntrusted || state == agentsetup.StateDisabled) {
			status.State, status.Reason, status.NextStep = state, result.Reason, result.NextStep
		}
	}
	status.HookCount = counts[status.State]
	return status, nil
}

func agentSetupCheckCommand(entry agentHookEntry) string {
	command, _ := entry.Fields["command"].(string)
	if args, ok := entry.Fields["args"].([]any); ok {
		for _, arg := range args {
			if s, ok := arg.(string); ok {
				command += " " + s
			}
		}
	}
	return command
}

func planAgentSetup(opts *nativeAgentHookOptions) (nativeAgentHookPlan, error) {
	plan, err := planNativeAgentHooks(*opts, false)
	if errors.Is(err, errMuseAttentionPermission) {
		opts.Attention = false
		return planNativeAgentHooks(*opts, false)
	}
	return plan, err
}

func agentSetupFallback(ctx context.Context, name string, checkOnly bool, items func(string) ([]agentsetup.Item, error)) (agentsetup.Row, error) {
	row := agentsetup.Row{Agent: name, AgentName: name, Piece: "hooks"}
	pieces, err := items(name)
	if err != nil {
		return row, err
	}
	item := pieces[0]
	if !checkOnly {
		if err := ctx.Err(); err != nil {
			return row, err
		}
		if err := item.Install(ctx); err != nil {
			row.Status = agentsetup.Status{State: agentsetup.StateFailed, Reason: err.Error(), NextStep: "kata agent setup " + name}
			return row, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return row, err
	}
	status, err := item.Check(ctx)
	if err != nil {
		row.Status = agentsetup.Status{State: agentsetup.StateFailed, Reason: err.Error(), NextStep: "kata agent setup " + name}
		return row, nil
	}
	if status.State != agentsetup.StateUnknown {
		status.State = agentsetup.StateUnknown
		status.Reason = "Hook inspection is unavailable for this agent"
		status.NextStep = "Inspect hooks in the agent"
	}
	row.Status = status
	return row, ctx.Err()
}

func newAgentSetupCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{Use: "setup [<harness>...]", Short: "Install and check agent hooks", RunE: func(cmd *cobra.Command, args []string) error {
		var warnings []string
		items := func(name string) ([]agentsetup.Item, error) {
			return agentSetupItem(name, func(warning string) { warnings = append(warnings, warning) })
		}
		var names []string
		for _, name := range args {
			capability, err := lookupAgentHookCapability(name)
			if err != nil {
				return err
			}
			if !slices.Contains(names, capability.Name) {
				names = append(names, capability.Name)
			}
		}
		var report agentsetup.Report
		var err error
		if len(names) == 0 {
			report, err = agentsetup.Run(cmd.Context(), agentsetup.Options{CheckOnly: check, SetupCommand: "kata agent setup", Items: items})
		} else {
			for _, name := range names {
				if err = cmd.Context().Err(); err != nil {
					break
				}
				if _, parseErr := agenthook.ParseAgent(name); parseErr == nil {
					var result agentsetup.Report
					result, err = agentsetup.Run(cmd.Context(), agentsetup.Options{Agents: []string{name}, CheckOnly: check, SetupCommand: "kata agent setup", Items: items})
					report.Rows = append(report.Rows, result.Rows...)
				} else {
					var row agentsetup.Row
					row, err = agentSetupFallback(cmd.Context(), name, check, items)
					report.Rows = append(report.Rows, row)
				}
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "or pass --agent", "or name an agent"))
		}
		for _, warning := range warnings {
			if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning:", warning); err != nil {
				return err
			}
		}
		for i := range report.Rows {
			if names, ok := strings.CutPrefix(report.Rows[i].NextStep, "kata agent setup --agent "); ok {
				report.Rows[i].NextStep = "kata agent setup " + strings.ReplaceAll(names, ",", " ")
			}
		}
		switch currentOutputMode() {
		case outputJSON:
			err = emitJSON(cmd.OutOrStdout(), report)
		case outputAgent:
			for _, row := range report.Rows {
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "agent=%s piece=%s state=%s reason=%s next_step=%s\n", agentValue(row.Agent), agentValue(row.Piece), row.State, agentValue(row.Reason), agentValue(row.NextStep)); err != nil {
					return err
				}
			}
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
