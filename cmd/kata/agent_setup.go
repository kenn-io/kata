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
	if len(targets) == 0 {
		return nativeAgentHookOptions{}, "", agentHookUsage(fmt.Sprintf("unknown agent %q", name))
	}
	opts := targets[0].options
	opts.Contract, opts.Attention = targets[0].capability.Contract, true
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
		if err != nil {
			return nativeAgentHookOptions{}, "", err
		}
	}
	opts.Executable = exe
	return opts, warning, nil
}

func agentSetupPlanCurrent(plan nativeAgentHookPlan) bool {
	for _, change := range plan.Changes {
		if change.OriginalExists == change.Remove || (!change.Remove && !bytes.Equal(change.Original, change.Content)) {
			return false
		}
	}
	return true
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

func agentSetupItem(name string, warn func(string)) agentsetup.Item {
	return agentsetup.Item{Name: "hooks", Install: func(ctx context.Context) error {
		opts, warning, err := agentSetupOptions(name)
		if errors.Is(err, errAgentSetupExecutables) {
			return nil
		}
		if err != nil {
			return err
		}
		plan, err := planAgentSetup(&opts)
		if err == nil && opts.Agent == "opencode" {
			if opts.API, _, err = resolveOpenCodeRuntimeAPI(ctx, plan.CurrentAPI); err == nil {
				plan, err = planNativeAgentHooks(opts, false)
			}
		}
		if err != nil {
			return err
		}
		if _, err := agenthook.ParseAgent(name); err != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		changed, err := publishNativeAgentHookPlan(plan)
		if err == nil && changed && warning != "" {
			warn(strings.Replace(warning, " (use --executable for a stable path)", ". Install Kata on PATH and rerun kata agent setup "+name, 1))
		}
		return err
	}, Check: func(ctx context.Context) (agentsetup.Status, error) {
		opts, _, err := agentSetupOptions(name)
		if errors.Is(err, errAgentSetupExecutables) {
			return agentsetup.Status{State: agentsetup.StateUnknown, Reason: err.Error(), NextStep: agentSetupRecovery(name)}, nil
		}
		if err != nil {
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
	}}
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
				return agentsetup.Status{State: agentsetup.StateUnknown, ConfigPath: opts.ConfigPath, Reason: "Agent executable is missing, so hook state is unknown", NextStep: "Install " + opts.Agent + " or add it to PATH"}, nil
			}
		}
	}
	status.ConfigPath = plan.Path
	if !agentSetupPlanCurrent(plan) {
		status.State, status.Reason, status.NextStep = agentsetup.StateMissing, "Kata hooks are not installed", "kata agent setup "+opts.Agent
		if plan.CurrentContract || plan.CurrentAttentionStart || plan.CurrentAttentionEnd {
			status.State, status.Reason = agentsetup.StateOutdated, "Kata hooks need updating"
		}
		if agentHookUsesKit(opts.Agent) {
			entries, err := inspectAgentHookEntries(agenthook.Agent(opts.Agent), opts.ConfigPath)
			if err != nil {
				return status, err
			}
			if slices.ContainsFunc(entries, func(entry agentHookEntry) bool { return classifyAgentHookHandlerFromEntry(entry) != "" }) {
				status.State, status.Reason = agentsetup.StateOutdated, "Kata hooks need updating"
			}
		}
		return status, nil
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
	conflict := agentsetup.Status{State: agentsetup.StateUnknown, ConfigPath: opts.ConfigPath, Reason: "Hook inspection found conflicting commands", NextStep: "Inspect hooks in the agent"}
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
		if registration.Marker == "" || slices.ContainsFunc(entries, func(entry agentHookEntry) bool {
			return classifyAgentHookHandlerFromEntry(entry) != kind && strings.Contains(agentSetupCheckCommand(entry), registration.Marker)
		}) {
			return conflict, nil
		}
		result, err := agenthook.Check(ctx, agenthook.Agent(opts.Agent), registration)
		if err != nil {
			return status, err
		}
		state := agentsetup.State(result.State)
		counts[state] += result.HookCount
		if state == agentsetup.StateMissing || state == agentsetup.StateOutdated {
			return conflict, nil
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
		if err == nil && row.State != agentsetup.StateUnknown {
			row.State = agentsetup.StateUnknown
		}
	}
	if err != nil {
		row.Status = agentsetup.Status{State: agentsetup.StateFailed, Reason: err.Error(), NextStep: "kata agent setup " + name}
	}
	return row, ctx.Err()
}

func newAgentSetupCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{Use: "setup [<harness>...]", Short: "Install and check agent hooks", RunE: func(cmd *cobra.Command, args []string) error {
		var warnings []string
		warn := func(warning string) { warnings = append(warnings, warning) }
		items := func(name string) ([]agentsetup.Item, error) {
			return []agentsetup.Item{agentSetupItem(name, warn)}, nil
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
		}
		// Kit runs its profiles; other selected profiles use Kata's installer.
		for _, name := range names {
			if _, parseErr := agenthook.ParseAgent(name); parseErr == nil {
				var result agentsetup.Report
				result, err = agentsetup.Run(cmd.Context(), agentsetup.Options{Agents: []string{name}, CheckOnly: check, SetupCommand: "kata agent setup", Items: items})
				report.Rows = append(report.Rows, result.Rows...)
			} else {
				var row agentsetup.Row
				row, err = agentSetupFallback(cmd.Context(), name, check, agentSetupItem(name, warn))
				report.Rows = append(report.Rows, row)
			}
			if err != nil {
				break
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
			report.Rows[i].NextStep = strings.Replace(report.Rows[i].NextStep, "kata agent setup --agent ", "kata agent setup ", 1)
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
