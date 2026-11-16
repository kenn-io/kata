package main

import (
	"context"
	"fmt"
	"os/exec"

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
