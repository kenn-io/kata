package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kit/agenthook"
)

type agentHookStatusEntry struct {
	Event            string `json:"event"`
	GroupIndex       int    `json:"group_index"`
	HandlerIndex     int    `json:"handler_index"`
	Matcher          string `json:"matcher"`
	Command          string `json:"command"`
	Executable       string `json:"executable"`
	ExecutableExists bool   `json:"executable_exists"`
	Kind             string `json:"kind"`
}

type agentHookUserStatus struct {
	ConfigPath string                 `json:"config_path"`
	Present    bool                   `json:"present"`
	Entries    []agentHookStatusEntry `json:"entries"`
}

type agentHookWorkspaceStatus struct {
	Path              string              `json:"path"`
	Claude            agentHookUserStatus `json:"claude"`
	Codex             agentHookUserStatus `json:"codex"`
	CommittedGuidance []string            `json:"committed_guidance"`
}

type agentHookHarnessStatus struct {
	Harness   string              `json:"harness"`
	User      agentHookUserStatus `json:"user"`
	Duplicate bool                `json:"duplicate"`
	Overlap   bool                `json:"overlap"`
}

type agentHookStatusReport struct {
	Harnesses []agentHookHarnessStatus `json:"harnesses"`
	Workspace agentHookWorkspaceStatus `json:"workspace"`
	Warnings  []string                 `json:"warnings"`
}

func newAgentHooksStatusCmd() *cobra.Command {
	var config string
	cmd := &cobra.Command{
		Use: "status [<harness>]", Short: "Inspect user hooks and the current workspace without a daemon",
		Long:              "Show user contract hooks, their executable paths, and current workspace contract/attention hooks.\n\nDuplicate means distinct user and workspace configs both inject the same harness's contract.\nOverlap means a user hook plus committed managed AGENTS.md or CLAUDE.md guidance; it is informational.\nUser scope is the default; --config requires exactly one harness. No files are changed.",
		ValidArgsFunction: agentHookUserCompletion(true),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return agentHookUsage("status accepts at most one harness")
			}
			if err := validateAgentHookConfigFlag(cmd, config); err != nil {
				return err
			}
			if config != "" && len(args) != 1 {
				return agentHookUsage("--config requires exactly one harness")
			}
			names := args
			if len(names) == 0 {
				for _, p := range sessionStartProfiles() {
					names = append(names, string(p.Agent))
				}
			}
			targets, err := selectAgentHookTargets(names, false, config)
			if err != nil {
				return err
			}
			workspace, err := agentHookWorkspacePath()
			if err != nil {
				return err
			}
			report, err := collectAgentHookStatus(targets, workspace)
			if err != nil {
				return err
			}
			return printAgentHookStatus(cmd, report)
		},
	}
	cmd.Flags().StringVar(&config, "config", "", "explicit config file; requires exactly one harness")
	return cmd
}

func readAgentHookStatus(agent agenthook.Agent, path, workspace string) (agentHookUserStatus, error) {
	status := agentHookUserStatus{ConfigPath: path, Entries: []agentHookStatusEntry{}}
	entries, err := inspectAgentHookEntries(agent, path)
	if err != nil {
		return status, err
	}
	status.Present = hasAgentHookContract(agent, entries)
	for _, entry := range entries {
		if !entry.Contract && !entry.Attention {
			continue
		}
		kind := "attention"
		if entry.Contract {
			kind = "contract"
		}
		goos := runtime.GOOS
		command, powershell := agentHookCommandForPlatform(agent, entry, goos)
		executable, parseErr := agentHookCommandExecutable(command, goos, powershell)
		exists := false
		if parseErr == nil {
			path := executable
			var lookupErr error
			if !filepath.IsAbs(path) {
				if strings.ContainsAny(path, "/\\") {
					path = filepath.Join(workspace, path)
				} else {
					path, lookupErr = exec.LookPath(path)
				}
			}
			if lookupErr == nil {
				info, err := os.Stat(path)
				exists = err == nil && info != nil && info.Mode().IsRegular()
			}
		}
		status.Entries = append(status.Entries, agentHookStatusEntry{
			Event: entry.Event, GroupIndex: entry.GroupIndex, HandlerIndex: entry.HandlerIndex,
			Matcher: entry.Matcher, Command: command, Executable: executable, ExecutableExists: exists, Kind: kind,
		})
	}
	return status, nil
}

func agentHookCommandForPlatform(agent agenthook.Agent, entry agentHookEntry, goos string) (string, bool) {
	commandField := func(name string) string {
		command, _ := entry.Fields[name].(string)
		return command
	}
	if goos == "windows" {
		if agent == agenthook.AgentCopilot {
			if command := commandField("powershell"); command != "" {
				return command, true
			}
		}
		if command := commandField("commandWindows"); command != "" {
			return command, false
		}
		if command := commandField("powershell"); command != "" {
			return command, true
		}
	} else if agent == agenthook.AgentCopilot {
		if command := commandField("bash"); command != "" {
			return command, false
		}
	}
	return entry.Command, false
}

func collectAgentHookStatus(targets []agentHookTarget, workspace string) (agentHookStatusReport, error) {
	report := agentHookStatusReport{Harnesses: []agentHookHarnessStatus{}, Workspace: agentHookWorkspaceStatus{Path: workspace}, Warnings: []string{}}
	var err error
	report.Workspace.Claude, err = readAgentHookStatus(agenthook.AgentClaude, filepath.Join(workspace, ".claude", "settings.json"), workspace)
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("inspect workspace Claude hooks: %v", err))
	}
	report.Workspace.Codex, err = readAgentHookStatus(agenthook.AgentCodex, filepath.Join(workspace, ".codex", "hooks.json"), workspace)
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("inspect workspace Codex hooks: %v", err))
	}
	report.Workspace.CommittedGuidance, err = readCommittedGuidance(workspace)
	if err != nil {
		report.Workspace.CommittedGuidance = []string{}
		report.Warnings = append(report.Warnings, fmt.Sprintf("inspect committed guidance: %v", err))
	}
	for _, target := range targets {
		user, err := readAgentHookStatus(target.Agent, target.ConfigPath, workspace)
		if err != nil {
			return report, err
		}
		status := agentHookHarnessStatus{Harness: string(target.Agent), User: user, Overlap: user.Present && len(report.Workspace.CommittedGuidance) > 0}
		var local *agentHookUserStatus
		switch target.Agent {
		case agenthook.AgentClaude:
			local = &report.Workspace.Claude
		case agenthook.AgentCodex:
			local = &report.Workspace.Codex
		}
		if local != nil {
			status.Duplicate = user.Present && local.Present && !sameAgentHookFile(user.ConfigPath, local.ConfigPath)
		}
		report.Harnesses = append(report.Harnesses, status)
	}
	return report, nil
}

// Overlap means committed team guidance, not a local or staged-only marker.
func readCommittedGuidance(workspace string) ([]string, error) {
	result := []string{}
	output, err := exec.Command("git", "-C", workspace, "rev-parse", "--show-toplevel").CombinedOutput() //nolint:gosec // G204: workspace is a directory argument to fixed read-only git commands; no shell.
	if err != nil {
		return nil, fmt.Errorf("inspect workspace git root: %w: %s", err, strings.TrimSpace(string(output)))
	}
	prefixOutput, err := exec.Command("git", "-C", workspace, "rev-parse", "--show-prefix").Output() //nolint:gosec // G204: workspace is a directory argument to fixed read-only Git commands; no shell.
	if err != nil {
		return nil, fmt.Errorf("inspect workspace git prefix: %w", err)
	}
	prefix := strings.TrimRight(string(prefixOutput), "\r\n")
	if _, err := exec.Command("git", "-C", workspace, "rev-parse", "--verify", "--quiet", "HEAD").Output(); err != nil { //nolint:gosec // G204: workspace and revision are fixed directory and Git arguments; no shell.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return result, nil
		}
		return nil, fmt.Errorf("inspect committed guidance HEAD: %w", err)
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		path := prefix + name
		tracked, err := exec.Command("git", "-C", workspace, "ls-tree", "-z", "HEAD", "--", ":(top)"+path).Output() //nolint:gosec // G204: fixed HEAD and literal guidance filenames passed as arguments, not shell code.
		if err != nil {
			return nil, err
		}
		if len(tracked) == 0 {
			continue
		}
		data, err := exec.Command("git", "-C", workspace, "show", "HEAD:"+path).Output() //nolint:gosec // G204: fixed HEAD and literal guidance filenames passed as arguments, not shell code.
		if err != nil {
			return nil, err
		}
		if bytes.Contains(data, []byte(agentsBlockBegin)) {
			result = append(result, name)
		}
	}
	return result, nil
}

func printAgentHookStatus(cmd *cobra.Command, report agentHookStatusReport) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), report)
	}
	if flags.Quiet {
		return nil
	}
	for _, warning := range report.Warnings {
		if currentOutputMode() == outputAgent {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "warning=%s\n", agentValue(warning)); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Warning: %s\n", textsafe.Line(warning)); err != nil {
			return err
		}
	}
	for _, harness := range report.Harnesses {
		if currentOutputMode() == outputAgent {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "harness=%s duplicate=%t overlap=%t\n", harness.Harness, harness.Duplicate, harness.Overlap); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: duplicate contract: %t; guidance overlap: %t\n", harness.Harness, harness.Duplicate, harness.Overlap); err != nil {
			return err
		}
		if err := printAgentHookScope(cmd, "user", harness.Harness, harness.User); err != nil {
			return err
		}
	}
	if currentOutputMode() == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "workspace=%s committed_guidance=%s\n", agentValue(report.Workspace.Path), agentValue(strings.Join(report.Workspace.CommittedGuidance, ","))); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Workspace: %s\n  Committed guidance: %s\n", textsafe.Line(report.Workspace.Path), textsafe.Line(strings.Join(report.Workspace.CommittedGuidance, ", "))); err != nil {
		return err
	}
	if err := printAgentHookScope(cmd, "workspace", "claude", report.Workspace.Claude); err != nil {
		return err
	}
	return printAgentHookScope(cmd, "workspace", "codex", report.Workspace.Codex)
}

func printAgentHookScope(cmd *cobra.Command, scope, harness string, status agentHookUserStatus) error {
	if currentOutputMode() == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "scope=%s harness=%s config_path=%s present=%t\n", scope, harness, agentValue(status.ConfigPath), status.Present); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s %s: %s (contract: %t)\n", scope, harness, textsafe.Line(status.ConfigPath), status.Present); err != nil {
		return err
	}
	for _, entry := range status.Entries {
		if currentOutputMode() == outputAgent {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  kind=%s event=%s group_index=%d handler_index=%d matcher=%s command=%s executable=%s executable_exists=%t\n", entry.Kind, agentValue(entry.Event), entry.GroupIndex, entry.HandlerIndex, agentValue(entry.Matcher), agentValue(entry.Command), agentValue(entry.Executable), entry.ExecutableExists); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(cmd.OutOrStdout(), "    %s on %s (group %d, handler %d)\n      Matcher: %s\n      Command: %s\n      Executable: %s (exists: %t)\n", entry.Kind, textsafe.Line(entry.Event), entry.GroupIndex, entry.HandlerIndex, textsafe.Line(entry.Matcher), textsafe.Line(entry.Command), textsafe.Line(entry.Executable), entry.ExecutableExists); err != nil {
			return err
		}
	}
	return nil
}
