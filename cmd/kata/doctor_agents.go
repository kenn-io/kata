package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.kenn.io/kata/internal/diagnostics"
	"go.kenn.io/kit/agentsetup"
)

func doctorAgent(ctx context.Context, name string) diagnostics.Check {
	return doctorAgentInWorkspace(ctx, name, "")
}

func doctorAgentInWorkspace(ctx context.Context, name, workspace string) diagnostics.Check {
	display := map[string]string{"claude": "Claude Code", "codex": "Codex"}[name]
	if _, err := exec.LookPath(name); err != nil {
		return diagnostics.Check{Status: diagnostics.StatusInfo, Summary: display + " is not installed"}
	}
	status, err := checkAgentSetupInWorkspace(ctx, name, workspace)
	if err != nil {
		return diagnostics.Check{Status: diagnostics.StatusFail, Summary: display + ": " + err.Error()}
	}
	result := diagnostics.Check{Status: diagnostics.StatusWarn, Summary: display + ": " + status.Reason, Fix: status.NextStep}
	switch status.State {
	case agentsetup.StateOK:
		result.Status = diagnostics.StatusOK
		result.Summary = display + ": Kata hooks are ready"
	case agentsetup.StateUntrusted:
		result.Summary = fmt.Sprintf("%s: %d Kata hooks wait for review", display, status.HookCount)
	}
	return result
}

func checkAgentSetupInWorkspace(ctx context.Context, name, workspace string) (agentsetup.Status, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return agentsetup.Status{}, err
	}
	if workspace == "" || filepath.Clean(workspace) == filepath.Clean(cwd) {
		return checkAgentSetup(ctx, name)
	}
	executable, err := os.Executable()
	if err != nil {
		return agentsetup.Status{}, err
	}
	cmd := exec.CommandContext(ctx, executable, "agent", "setup", "--check", "--json", name)
	cmd.Dir = workspace
	out, runErr := cmd.Output()
	var report agentsetup.Report
	if err := json.Unmarshal(out, &report); err != nil {
		if runErr != nil {
			return agentsetup.Status{}, fmt.Errorf("check agent setup: %w", runErr)
		}
		return agentsetup.Status{}, fmt.Errorf("decode agent setup: %w", err)
	}
	if len(report.Rows) != 1 {
		return agentsetup.Status{}, fmt.Errorf("check agent setup: expected one result, got %d", len(report.Rows))
	}
	return report.Rows[0].Status, nil
}
