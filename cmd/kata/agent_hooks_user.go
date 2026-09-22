package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kit/pathresolve"

	"go.kenn.io/kit/agenthook"
)

type agentHookTarget struct {
	Agent      agenthook.Agent
	ConfigPath string
	SkipReason string
}

func sessionStartProfiles() []agenthook.Profile {
	var result []agenthook.Profile
	for _, p := range agenthook.Profiles() {
		if slices.Contains(p.SupportedEvents, agenthook.EventSessionStart) {
			result = append(result, p)
		}
	}
	return result
}

func contractRegistrationHook(agent agenthook.Agent) agenthook.Hook {
	if agent == agenthook.AgentHermes {
		return agenthook.Hook{Event: agenthook.EventUserPromptSubmit, Timeout: 10 * time.Second}
	}
	hook := agenthook.Hook{Event: agenthook.EventSessionStart, Timeout: 10 * time.Second}
	if agent == agenthook.AgentCodex {
		hook.Matcher = codexContractSessionStartMatcher
	}
	return hook
}

func selectAgentHookTargets(names []string, all bool, config string) ([]agentHookTarget, error) {
	if all && (len(names) > 0 || config != "") {
		return nil, agentHookUsage("--all cannot be combined with harness names or --config")
	}
	if !all && len(names) == 0 {
		return nil, agentHookUsage("name at least one harness or use --all")
	}
	if config != "" && len(names) != 1 {
		return nil, agentHookUsage("--config requires exactly one harness")
	}
	var profiles []agenthook.Profile
	if all {
		profiles = agenthook.Profiles()
	} else {
		seen := map[agenthook.Agent]bool{}
		for _, name := range names {
			agent, err := parseAgentHookHarness(name)
			if err != nil {
				return nil, err
			}
			if seen[agent] {
				continue
			}
			seen[agent] = true
			p, _ := agenthook.LookupProfile(agent)
			profiles = append(profiles, p)
		}
	}
	targets := make([]agentHookTarget, 0, len(profiles))
	for _, p := range profiles {
		path := config
		if path == "" {
			var err error
			path, err = agenthook.ConfigPath(p.Agent)
			if err != nil {
				return nil, err
			}
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		target := agentHookTarget{Agent: p.Agent, ConfigPath: path}
		if all {
			if !slices.Contains(p.SupportedEvents, agenthook.EventSessionStart) {
				target.SkipReason = "no SessionStart"
			} else {
				root := path
				for name := filepath.Clean(p.ConfigFilename); name != "."; name = filepath.Dir(name) {
					root = filepath.Dir(root)
				}
				info, err := os.Stat(root)
				switch {
				case errors.Is(err, os.ErrNotExist):
					target.SkipReason = "not installed"
				case err != nil:
					return nil, fmt.Errorf("inspect %s config root: %w", p.Agent, err)
				case !info.IsDir():
					return nil, fmt.Errorf("%s config root %s is not a directory", p.Agent, root)
				}
			}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

const codexAgentHookTrustNote = "Codex runs new hooks only after you trust them: open Codex and run /hooks."

type agentHookMutation struct {
	Harness    string   `json:"harness"`
	ConfigPath string   `json:"config_path"`
	Changed    bool     `json:"changed"`
	State      string   `json:"state"`
	Reason     string   `json:"reason"`
	Warnings   []string `json:"warnings"`
}

type agentHookInstaller func(agenthook.Agent, agenthook.InstallOptions) (agenthook.Result, error)

func newAgentHooksInstallCmd() *cobra.Command {
	return newAgentHooksInstallCmdWithInstaller(agenthook.Install)
}

func newAgentHooksInstallCmdWithInstaller(install agentHookInstaller) *cobra.Command {
	var all bool
	var config, executable string
	cmd := &cobra.Command{
		Use: "install (<harness>... | --all)", Short: "Install the contract hook in user coding-agent configs",
		Long:              "Install the contract in every session for the named coding agents. User scope is the default and only scope.\n\n--all writes only where harness config roots already exist. Hermes uses first-turn pre_llm_call; other harnesses use SessionStart.\nUse --config with exactly one harness to select another config home. Codex requires trust through /hooks.",
		ValidArgsFunction: agentHookUserCompletion(false),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateAgentHookConfigFlag(cmd, config); err != nil {
				return err
			}
			if cmd.Flags().Changed("executable") && executable == "" {
				return agentHookUsage("--executable must not be empty")
			}
			targets, err := selectAgentHookTargets(args, all, config)
			if err != nil {
				return err
			}
			path, warning, err := resolveAgentHookExecutable(executable, agentHookExecutableEnv{Executable: os.Executable, EvalSymlinks: pathresolve.EvalSymlinks, Path: os.Getenv("PATH"), GOOS: runtime.GOOS})
			if err != nil {
				return err
			}
			return runAgentHookInstall(cmd, targets, path, warning, install)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "select every supported harness with an existing config root")
	cmd.Flags().StringVar(&config, "config", "", "explicit config file; requires exactly one harness")
	cmd.Flags().StringVar(&executable, "executable", "", "existing executable path or command on PATH to install")
	return cmd
}

func validateAgentHookConfigFlag(cmd *cobra.Command, config string) error {
	if cmd.Flags().Changed("config") && config == "" {
		return agentHookUsage("--config must not be empty")
	}
	return nil
}

func agentHookUserCompletion(single bool) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		var names []string
		if !single || len(args) == 0 {
			for _, p := range sessionStartProfiles() {
				name := string(p.Agent)
				already := false
				for _, arg := range args {
					if strings.EqualFold(arg, name) {
						already = true
					}
				}
				if !already && strings.HasPrefix(name, prefix) {
					names = append(names, name)
				}
			}
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}

type agentHookInstallPlan struct {
	Target   agentHookTarget
	Options  agenthook.InstallOptions
	Result   agenthook.Result
	Same     bool
	HadOwned bool
	Warnings []string
}

func runAgentHookInstall(cmd *cobra.Command, targets []agentHookTarget, executable, warning string, install agentHookInstaller) error {
	workspace, err := agentHookWorkspacePath()
	if err != nil {
		return err
	}
	plans := make([]agentHookInstallPlan, 0, len(targets))
	for _, target := range targets {
		plan := agentHookInstallPlan{Target: target, Warnings: []string{}}
		if target.SkipReason != "" {
			plans = append(plans, plan)
			continue
		}
		plan.Options = agenthook.InstallOptions{ConfigPath: target.ConfigPath, Executable: executable,
			Arguments: []string{"agent-hooks", "contract", string(target.Agent), "--source", agentContractHookSource},
			Marker:    agentContractMarker, Hooks: []agenthook.Hook{contractRegistrationHook(target.Agent)}}
		plan.Result, err = agenthook.PlanInstall(target.Agent, plan.Options)
		if err != nil {
			return err
		}
		current, err := inspectAgentHookEntries(target.Agent, target.ConfigPath)
		if err != nil {
			return err
		}
		expected, err := parseAgentHookEntries(target.Agent, plan.Result.Data)
		if err != nil {
			return err
		}
		plan.Same = ownedContractMatches(current, expected)
		for _, entry := range current {
			plan.HadOwned = plan.HadOwned || entry.Contract
		}
		if warning != "" {
			plan.Warnings = append(plan.Warnings, warning)
		}
		hint, err := agentHookDuplicateHint(target.Agent, target.ConfigPath, workspace)
		if err != nil {
			plan.Warnings = append(plan.Warnings, err.Error())
		}
		if hint != "" {
			plan.Warnings = append(plan.Warnings, hint)
		}
		plans = append(plans, plan)
	}
	results := make([]agentHookMutation, 0, len(plans))
	for _, plan := range plans {
		result := agentHookMutation{Harness: string(plan.Target.Agent), ConfigPath: plan.Target.ConfigPath, State: "unchanged", Warnings: plan.Warnings}
		if plan.Target.SkipReason != "" {
			result.State = "skipped"
			result.Reason = plan.Target.SkipReason
		} else if !plan.Same {
			installed, err := install(plan.Target.Agent, plan.Options)
			if err != nil {
				return agentHookWriteError(result, results, err)
			}
			result.Changed = installed.Changed
			if result.Changed {
				result.State = "installed"
				if plan.HadOwned {
					result.Warnings = append(result.Warnings, "replaced owned contract hooks; the harness may ask to re-trust")
				}
				if plan.Target.Agent == agenthook.AgentCodex {
					result.Warnings = append(result.Warnings, codexAgentHookTrustNote)
				}
			}
		}
		results = append(results, result)
	}
	return printAgentHookMutations(cmd, "install", results)
}

func agentHookWriteError(current agentHookMutation, previous []agentHookMutation, err error) error {
	var changed, warnings []string
	for _, result := range previous {
		if result.Changed {
			changed = append(changed, result.ConfigPath)
			warnings = append(warnings, result.Warnings...)
		}
	}
	if len(changed) > 0 {
		detail := "earlier configs changed: " + strings.Join(changed, ", ")
		if len(warnings) > 0 {
			detail += "; earlier warnings: " + strings.Join(warnings, "; ")
		}
		return fmt.Errorf("write %s config %s (%s): %w", current.Harness, current.ConfigPath, detail, err)
	}
	return fmt.Errorf("write %s config %s: %w", current.Harness, current.ConfigPath, err)
}

func printAgentHookMutations(cmd *cobra.Command, verb string, results []agentHookMutation) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), struct {
			Action  string              `json:"action"`
			Results []agentHookMutation `json:"results"`
		}{verb, results})
	}
	if flags.Quiet {
		return nil
	}
	for _, result := range results {
		if currentOutputMode() == outputAgent {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "harness=%s config_path=%s state=%s changed=%t reason=%s\n", agentValue(result.Harness), agentValue(result.ConfigPath), result.State, result.Changed, agentValue(result.Reason)); err != nil {
				return err
			}
		} else {
			state := result.State
			if result.Reason != "" {
				state += " (" + result.Reason + ")"
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s — %s\n", result.Harness, state, textsafe.Line(result.ConfigPath)); err != nil {
				return err
			}
		}
		for _, warning := range result.Warnings {
			if currentOutputMode() == outputAgent {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "warning=%s\n", agentValue(warning)); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintln(cmd.OutOrStdout(), textsafe.Line(warning)); err != nil {
				return err
			}
		}
	}
	return nil
}

func newAgentHooksUninstallCmd() *cobra.Command {
	var all bool
	var config string
	cmd := &cobra.Command{
		Use: "uninstall (<harness>... | --all)", Short: "Remove user contract hooks while preserving foreign hooks",
		Long:              "Remove marker-owned contract hooks from user configs through Kit. Missing configs are a successful no-op.\n\nUser scope is the default and only scope; --config requires exactly one harness.\nWorkspaces that skipped their Codex contract hook may need kata init --with-codex-hooks afterward.",
		ValidArgsFunction: agentHookUserCompletion(false),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateAgentHookConfigFlag(cmd, config); err != nil {
				return err
			}
			targets, err := selectAgentHookTargets(args, all, config)
			if err != nil {
				return err
			}
			// Preflight all configs before publishing any removal.
			for _, target := range targets {
				if target.SkipReason != "" {
					continue
				}
				if _, err := agenthook.PlanUninstall(target.Agent, target.ConfigPath, agentContractMarker); err != nil {
					return err
				}
			}
			results := make([]agentHookMutation, 0, len(targets))
			for _, target := range targets {
				result := agentHookMutation{Harness: string(target.Agent), ConfigPath: target.ConfigPath, State: "absent", Warnings: []string{}}
				if target.SkipReason != "" {
					result.State = "skipped"
					result.Reason = target.SkipReason
				} else {
					removed, err := agenthook.Uninstall(target.Agent, target.ConfigPath, agentContractMarker)
					if err != nil {
						return agentHookWriteError(result, results, err)
					}
					result.Changed = removed.Changed
					if result.Changed {
						result.State = "removed"
						if target.Agent == agenthook.AgentCodex {
							result.Warnings = append(result.Warnings,
								"workspaces initialized while the user hook existed may have no contract hook; run kata init --with-codex-hooks there to restore it")
						}
					}
				}
				results = append(results, result)
			}
			return printAgentHookMutations(cmd, "uninstall", results)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "select every supported harness with an existing config root")
	cmd.Flags().StringVar(&config, "config", "", "explicit config file; requires exactly one harness")
	return cmd
}
