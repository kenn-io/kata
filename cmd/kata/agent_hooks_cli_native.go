package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"
)

type nativeAgentHookTarget struct {
	capability agentHookCapability
	options    nativeAgentHookOptions
	path, skip string
}

func selectNativeAgentHookTargets(names []string, all bool, scope, config, dir string) ([]nativeAgentHookTarget, error) {
	if all && (len(names) > 0 || config != "") {
		return nil, agentHookUsage("--all cannot be combined with harness names or --config")
	}
	automatic := all || len(names) == 0
	if config != "" && len(names) != 1 {
		return nil, agentHookUsage("--config requires exactly one harness")
	}
	if scope != "user" && scope != "project" {
		return nil, agentHookUsage("--scope must be user or project")
	}
	if automatic {
		for _, capability := range agentHookCapabilities() {
			names = append(names, capability.Name)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var targets []nativeAgentHookTarget
	seen := map[string]bool{}
	for _, name := range names {
		capability, err := lookupAgentHookCapability(name)
		if err != nil {
			return nil, err
		}
		if seen[capability.Name] {
			continue
		}
		seen[capability.Name] = true
		target := nativeAgentHookTarget{capability: capability, options: nativeAgentHookOptions{Agent: capability.Name, Scope: scope, Home: home, Dir: dir}}
		if scope == "project" && !capability.Project && automatic {
			target.skip = "no verified project hook discovery"
			targets = append(targets, target)
			continue
		}
		target.path, err = agentHookScopePath(capability.Name, scope, dir)
		if err != nil {
			return nil, err
		}
		if config != "" {
			if capability.Name == "pi" || capability.Name == "amp" || (capability.Name == "opencode" && scope == "project") {
				return nil, agentHookUsage(fmt.Sprintf("%s uses auto-discovery; --config is unsupported for this scope", capability.Name))
			}
			target.path, err = filepath.Abs(config)
			if err != nil {
				return nil, err
			}
			target.options.ConfigPath = target.path
		}
		if agentHookUsesKit(capability.Name) {
			target.options.ConfigPath = target.path
		}
		if automatic {
			configured, err := nativeAgentHookConfiguredTarget(target)
			if err != nil {
				return nil, err
			}
			if !configured {
				target.skip = "not configured"
			}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func nativeAgentHookConfigRoot(name, scope, path string) string {
	if agentHookUsesKit(name) && scope == "user" {
		profile, _ := agenthook.LookupProfile(agenthook.Agent(name))
		root := path
		for filename := filepath.Clean(profile.ConfigFilename); filename != "."; filename = filepath.Dir(filename) {
			root = filepath.Dir(root)
		}
		return root
	}
	root := filepath.Dir(path)
	switch name {
	case "pi", "amp", "opencode", "grok":
		root = filepath.Dir(root)
	case "openclaw":
		root = filepath.Dir(root)
	}
	if name == "copilot" && scope == "project" {
		root = filepath.Dir(root)
	}
	return root
}

func planNativeAgentHooks(opts nativeAgentHookOptions, remove bool) (nativeAgentHookPlan, error) {
	if agentHookUsesKit(opts.Agent) {
		return planKitAgentHooks(opts, remove)
	}
	switch opts.Agent {
	case "pi":
		return planPiAgentHooks(opts, remove)
	case "amp":
		return planAmpAgentHooks(opts, remove)
	case "opencode":
		return planOpenCodeAgentHooks(opts, remove)
	case "openclaw":
		return planOpenClawAgentHooks(opts, remove)
	case "droid", "antigravity", "zcode", "kimi-code", "kimi", "muse", "grok":
		return planExtraAgentHooks(opts, remove)
	}
	return nativeAgentHookPlan{}, agentHookUsage("native provider unavailable for " + opts.Agent)
}

func newNativeAgentHooksMutationCmd(remove bool) *cobra.Command {
	return newNativeAgentHooksMutationCmdWithTerminalCheck(remove, agentHookInputIsTerminal)
}

func newNativeAgentHooksMutationCmdWithTerminalCheck(remove bool, isTerminal func(io.Reader) bool) *cobra.Command {
	var all, attention, managed, local, contractOnly bool
	var scope, config, executable, source, api string
	verb := "install"
	short := "Install contract and available attention hooks for configured or named agents"
	if remove {
		verb = "uninstall"
		short = "Remove owned contract and attention hooks"
	}
	cmd := &cobra.Command{Use: verb + " [<harness>...]", Short: short, ValidArgsFunction: agentHookUserCompletion(false)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if remove && !all && len(args) == 0 {
			return agentHookUsage("name at least one harness or use --all")
		}
		var err error
		scope, err = resolveNativeAgentHookScope(cmd, scope, local)
		if err != nil {
			return err
		}
		attention, err = resolveNativeAgentHookAttention(cmd, attention, contractOnly, managed)
		if err != nil {
			return err
		}
		if err := validateAgentHookConfigFlag(cmd, config); err != nil {
			return err
		}
		if !remove && cmd.Flags().Changed("executable") && executable == "" {
			return agentHookUsage("--executable must not be empty")
		}
		if !remove && cmd.Flags().Changed("source") && source == "" {
			return agentHookUsage("--source must not be empty")
		}
		if !remove && cmd.Flags().Changed("source") && scope == "user" && !filepath.IsAbs(source) {
			source, err = filepath.Abs(source)
			if err != nil {
				return fmt.Errorf("resolve user-scope --source: %w", err)
			}
		}
		dir, err := agentHookWorkspacePath()
		if err != nil {
			return err
		}
		targets, err := selectNativeAgentHookTargets(args, all, scope, config, dir)
		if err != nil {
			return err
		}
		if !all && len(args) == 0 {
			configured := false
			for _, target := range targets {
				configured = configured || target.skip == ""
			}
			if !configured {
				return agentHookUsage("no configured agents found; no setup performed. Name agents explicitly, for example: kata agent-hooks install codex pi")
			}
		}
		automatic := all || len(args) == 0
		if automatic && !remove && !attention {
			for i := range targets {
				if targets[i].skip == "" && !targets[i].capability.Contract {
					targets[i].skip = "attention only; use --attention"
				}
			}
		}
		if !remove && cmd.Flags().Changed("api") && (len(targets) != 1 || targets[0].capability.Name != "opencode" || (api != "v1" && api != "v2")) {
			return agentHookUsage("--api v1|v2 requires exactly one OpenCode target")
		}
		if !remove && managed && (!attention || automatic || len(targets) != 1 || targets[0].capability.Name != "muse" || scope != "user") {
			return agentHookUsage("--managed-attention requires exactly one explicit Muse target in user scope")
		}
		for _, target := range targets {
			if target.skip != "" {
				continue
			}
			if !remove && !target.capability.Contract && !attention {
				return agentHookUsage(target.capability.Name + " supports attention only; select --attention and use committed AGENTS.md for the contract")
			}
			if !remove && cmd.Flags().Changed("source") && target.capability.Name != "pi" && target.capability.Name != "openclaw" && target.capability.Name != "amp" && target.capability.Name != "opencode" {
				return agentHookUsage("--source is supported only by owned native code adapters")
			}
		}
		warning := ""
		if !remove {
			executable, warning, err = resolveAgentHookExecutable(executable, defaultAgentHookExecutableEnv())
			if err != nil {
				return err
			}
		}
		results := make([]agentHookMutation, 0, len(targets))
		combined := nativeAgentHookPlan{}
		for _, target := range targets {
			result := agentHookMutation{Harness: target.capability.Name, ConfigPath: target.path, State: "unchanged", Warnings: []string{}}
			if target.skip != "" {
				result.State = "skipped"
				result.Reason = target.skip
				results = append(results, result)
				continue
			}
			opts := target.options
			opts.Executable = executable
			opts.Contract = target.capability.Contract
			opts.Attention = attention
			opts.ManagedAttention = managed
			opts.API = api
			opts.Source = source
			opts.SourceSet = !remove && cmd.Flags().Changed("source")
			if !remove && opts.Agent == "opencode" {
				// Pure install planning checks authored collisions before a probe can
				// skip this target. An unset API retains any owned SDK/layout choice.
				inspection := opts
				inspection.API = ""
				if _, err := planNativeAgentHooks(inspection, false); err != nil {
					return err
				}
				var runtimeWarning string
				opts.API, runtimeWarning, err = resolveOpenCodeRuntimeAPI(cmd.Context(), api)
				if err != nil {
					if !automatic {
						return err
					}
					result.State, result.Reason = "skipped", err.Error()
					results = append(results, result)
					continue
				}
				if runtimeWarning != "" {
					result.Warnings = append(result.Warnings, runtimeWarning)
				}
			}
			plan, err := planNativeAgentHooks(opts, remove)
			if !remove && errors.Is(err, errMuseAttentionPermission) {
				canPrompt := !automatic && len(targets) == 1 && scope == "user" && currentOutputMode() == outputHuman && !flags.Quiet && !cmd.Flags().Changed("managed-attention") && isTerminal(cmd.InOrStdin())
				if canPrompt {
					if _, promptErr := fmt.Fprint(cmd.ErrOrStderr(), "Muse clears hook environments. Its user-wide managed_hooks_env_vars allowlist is forwarded to every managed hook, including hooks from other tools. Allow all managed hooks to receive Kata issue, routing, identity and authentication variables, including KATA_AUTH_TOKEN when set, from your launching environment? Only variable names are stored; this policy is retained on uninstall. [y/N] "); promptErr != nil {
						return promptErr
					}
					answer, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
					if readErr != nil && !errors.Is(readErr, io.EOF) {
						return readErr
					}
					answer = strings.TrimSpace(answer)
					opts.ManagedAttention = readErr == nil && (strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"))
				}
				if !opts.ManagedAttention {
					opts.Attention = false
					result.Reason = "contract configured; attention permission needed: kata agent-hooks install muse --managed-attention (user scope)"
				}
				plan, err = planNativeAgentHooks(opts, false)
			}
			if err != nil {
				return err
			}
			result.ConfigPath = plan.Path
			result.Warnings = append(result.Warnings, plan.Warnings...)
			if target.capability.Note != "" {
				result.Warnings = append(result.Warnings, target.capability.Note)
			}
			if warning != "" {
				result.Warnings = append(result.Warnings, warning)
			}
			for _, change := range plan.Changes {
				result.Changed = result.Changed || (change.Remove && change.OriginalExists) || (!change.Remove && (!change.OriginalExists || !bytes.Equal(change.Original, change.Content)))
			}
			if remove {
				result.State = "absent"
				if result.Changed {
					result.State = "removed"
				}
			} else if result.Changed {
				result.State = "installed"
			}
			if !remove && result.Reason != "" {
				result.State = "partial"
			}
			if !remove && agentHookUsesKit(opts.Agent) {
				hint, err := agentHookDuplicateHint(agenthook.Agent(opts.Agent), plan.Path, dir)
				if err != nil {
					result.Warnings = append(result.Warnings, err.Error())
				}
				if hint != "" {
					result.Warnings = append(result.Warnings, hint)
				}
				if result.Changed && plan.CurrentOwnedContract {
					result.Warnings = append(result.Warnings, "replaced owned contract hooks; the harness may ask to re-trust")
				}
				if result.Changed && opts.Agent == "codex" {
					result.Warnings = append(result.Warnings, codexAgentHookTrustNote)
				}
			}
			if remove && result.Changed && opts.Agent == "codex" {
				result.Warnings = append(result.Warnings, "workspaces initialized while the user hook existed may have no contract hook; run kata init --with-codex-hooks there to restore it")
			}
			combined.Changes = append(combined.Changes, plan.Changes...)
			results = append(results, result)
		}
		if _, err := publishNativeAgentHookPlan(combined); err != nil {
			return fmt.Errorf("publish hook bundle: %w", err)
		}
		return printAgentHookMutations(cmd, verb, results)
	}
	cmd.Flags().BoolVar(&all, "all", false, "discover configured harnesses (also the install default)")
	cmd.Flags().BoolVar(&local, "local", false, "use project scope in the selected workspace")
	cmd.Flags().BoolVar(&contractOnly, "contract-only", false, "select only contract hooks; preserve existing attention on install")
	cmd.Flags().StringVar(&scope, "scope", "user", "installation scope: user or project")
	cmd.Flags().StringVar(&config, "config", "", "explicit native config file; requires exactly one supported command target")
	cmd.Flags().BoolVar(&attention, "attention", true, "include available attention directions; --attention=false selects contract only")
	if !remove {
		cmd.Flags().StringVar(&executable, "executable", "", "existing executable path or command on PATH to install")
		cmd.Flags().StringVar(&source, "source", "", "contract prompt file for an owned native code adapter")
		cmd.Flags().StringVar(&api, "api", "", "advanced OpenCode API override: v1 or v2 (default: probe opencode --version)")
		cmd.Flags().BoolVar(&managed, "managed-attention", false, "grant Muse global forwarding of Kata variables, including KATA_AUTH_TOKEN, to all Muse managed hooks")
	}
	return cmd
}

func nativeHookNames(prefix string, args []string) []string {
	var names []string
	for _, capability := range agentHookCapabilities() {
		already := false
		for _, arg := range args {
			canonical, _ := canonicalNativeAttentionTarget(arg)
			already = already || canonical == capability.Name
		}
		if !already && strings.HasPrefix(capability.Name, prefix) {
			names = append(names, capability.Name)
		}
	}
	return names
}

func newAgentHooksStatusCmd() *cobra.Command {
	var scope, config string
	var local bool
	cmd := &cobra.Command{Use: "status [<harness>]", Short: "Inspect configured contract and lifecycle capabilities without a daemon", ValidArgsFunction: agentHookUserCompletion(true)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var err error
		scope, err = resolveNativeAgentHookScope(cmd, scope, local)
		if err != nil {
			return err
		}
		if len(args) > 1 {
			return agentHookUsage("status accepts at most one harness")
		}
		if err := validateAgentHookConfigFlag(cmd, config); err != nil {
			return err
		}
		if config != "" && len(args) != 1 {
			return agentHookUsage("--config requires exactly one harness")
		}
		dir, err := agentHookWorkspacePath()
		if err != nil {
			return err
		}
		scopes := []string{scope}
		if len(args) == 0 && !cmd.Flags().Changed("scope") && !cmd.Flags().Changed("local") {
			scopes = []string{"user", "project"}
		}
		var targets []nativeAgentHookTarget
		for _, selectedScope := range scopes {
			names := append([]string(nil), args...)
			if len(names) == 0 {
				for _, capability := range agentHookCapabilities() {
					if selectedScope != "project" || capability.Project {
						names = append(names, capability.Name)
					}
				}
			}
			selected, err := selectNativeAgentHookTargets(names, false, selectedScope, config, dir)
			if err != nil {
				return err
			}
			targets = append(targets, selected...)
		}
		report, err := collectAgentHookStatus(nil, dir)
		if err != nil {
			return err
		}
		report.Warnings = append(report.Warnings, "Offline status reports owned configured artifacts; it cannot confirm native loading, hook trust, or runtime permissions.")
		for _, target := range targets {
			scope := target.options.Scope
			plan, err := planNativeAgentHooks(target.options, true)
			if err != nil {
				path := plan.Path
				if path == "" {
					path = target.path
				}
				report.Harnesses = append(report.Harnesses, agentHookHarnessStatus{Harness: target.capability.Name, Scope: scope, Capabilities: target.capability, InspectionError: err.Error(), User: agentHookUserStatus{ConfigPath: path, Entries: []agentHookStatusEntry{}}})
				report.Warnings = append(report.Warnings, target.capability.Name+": "+err.Error())
				continue
			}
			user := agentHookUserStatus{ConfigPath: plan.Path, Present: plan.CurrentContract, Entries: []agentHookStatusEntry{}}
			if agentHookUsesKit(target.capability.Name) {
				user, err = readAgentHookStatus(agenthook.Agent(target.capability.Name), plan.Path, dir)
				if err != nil {
					report.Harnesses = append(report.Harnesses, agentHookHarnessStatus{Harness: target.capability.Name, Scope: scope, Capabilities: target.capability, InspectionError: err.Error(), User: user})
					report.Warnings = append(report.Warnings, target.capability.Name+": "+err.Error())
					continue
				}
			}
			row := agentHookHarnessStatus{Harness: target.capability.Name, Scope: scope, Capabilities: target.capability, Configured: agentHookConfigured{Contract: plan.CurrentContract, AttentionStart: plan.CurrentAttentionStart, AttentionEnd: plan.CurrentAttentionEnd}, User: user, Overlap: scope == "user" && user.Present && len(report.Workspace.CommittedGuidance) > 0}
			if scope == "user" {
				switch target.capability.Name {
				case "claude":
					row.Duplicate = user.Present && report.Workspace.Claude.Present && !sameAgentHookFile(plan.Path, report.Workspace.Claude.ConfigPath)
				case "codex":
					row.Duplicate = user.Present && report.Workspace.Codex.Present && !sameAgentHookFile(plan.Path, report.Workspace.Codex.ConfigPath)
				}
			}
			report.Harnesses = append(report.Harnesses, row)
			report.Warnings = append(report.Warnings, plan.Warnings...)
			if target.capability.Note != "" {
				report.Warnings = append(report.Warnings, target.capability.Name+": "+target.capability.Note)
			}
		}
		return printAgentHookStatus(cmd, report)
	}
	cmd.Flags().BoolVar(&local, "local", false, "inspect project scope in the selected workspace")
	cmd.Flags().StringVar(&scope, "scope", "user", "configured scope: user or project (bare status shows both)")
	cmd.Flags().StringVar(&config, "config", "", "explicit native config file; requires exactly one supported command target")
	return cmd
}
