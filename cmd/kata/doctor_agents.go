package main

import (
	"context"
	"fmt"
	"os/exec"

	"go.kenn.io/kata/internal/diagnostics"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentsetup"
)

func doctorAgent(ctx context.Context, name string) diagnostics.Check {
	display := map[string]string{"claude": "Claude Code", "codex": "Codex"}[name]
	_, lookupErr := exec.LookPath(name)
	opts, _, err := agentSetupOptions(name)
	if err == errAgentSetupExecutables {
		return diagnostics.Check{Status: diagnostics.StatusWarn, Summary: display + ": " + err.Error(), Fix: agentSetupRecovery(name)}
	}
	if err != nil {
		return diagnostics.Check{Status: diagnostics.StatusFail, Summary: display + ": " + err.Error()}
	}
	if lookupErr != nil {
		configured, err := readAgentHookStatus(agenthook.Agent(name), opts.ConfigPath, opts.Dir)
		if err != nil {
			return diagnostics.Check{Status: diagnostics.StatusFail, Summary: display + ": " + err.Error()}
		}
		if len(configured.Entries) == 0 {
			return diagnostics.Check{Status: diagnostics.StatusInfo, Summary: display + " is not installed"}
		}
		return diagnostics.Check{Status: diagnostics.StatusWarn, Summary: display + ": hook state is unknown because the agent is missing", Fix: "Install " + display + " or add it to PATH"}
	}
	probe, cancel := context.WithTimeout(ctx, doctorRequestTimeout)
	defer cancel()
	plan, err := planAgentSetup(&opts)
	if err != nil {
		return diagnostics.Check{Status: diagnostics.StatusFail, Summary: display + ": " + err.Error()}
	}
	status, err := checkAgentSetup(probe, opts, plan)
	if err != nil {
		return diagnostics.Check{Status: diagnostics.StatusFail, Summary: display + ": " + err.Error()}
	}
	result := diagnostics.Check{Status: diagnostics.StatusWarn, Summary: display + ": " + status.Reason, Fix: status.NextStep}
	switch status.State {
	case agentsetup.StateOK:
		result.Status = diagnostics.StatusOK
		result.Summary = display + ": Kata hooks are ready"
	case agentsetup.StateFailed:
		result.Status = diagnostics.StatusFail
	case agentsetup.StateUntrusted:
		result.Summary = fmt.Sprintf("%s: %d Kata hooks wait for review", display, status.HookCount)
	}
	return result
}
