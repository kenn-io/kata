package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/telemetry"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// cliUseReportTimeout bounds the use request if the process outlives the exit wait;
// var so tests can shorten it.
var cliUseReportTimeout = time.Second

// agentUseReportTimeout bounds hook requests if the process outlives the exit wait.
var agentUseReportTimeout = 100 * time.Millisecond

// cliUseNow picks the UTC day a report counts for; var so tests can move it.
var cliUseNow = time.Now

// cliUseRetryBackoff spaces attempts whose outcome the process never observed.
const cliUseRetryBackoff = 15 * time.Minute

// cliUseTarget is the resolved daemon this invocation built its own client from.
var cliUseTarget atomic.Pointer[client.ResolvedDaemon]

// agentFacingCommands run for agents or hook runners, not a person at a prompt.
var agentFacingCommands = []string{"mcp serve", "agent-hook", "attention-hook", "agent-contract-hook"}

func recordCLIUseTarget(resolved client.ResolvedDaemon) {
	cliUseTarget.Store(&resolved)
}

// reportsCLIUse reports whether a successful cmd counts as a person's use.
func reportsCLIUse(cmd *cobra.Command) bool {
	// A person at a prompt reads stdout on a terminal; agents and scripts read it through a pipe.
	if !isTTY(os.Stdout) {
		return false
	}
	if mode := currentOutputMode(); mode == outputAgent || mode == outputContract {
		return false
	}
	if os.Getenv(hooks.HookVersionEnv) != "" {
		return false
	}
	// --context emits bounded output for an agent harness, such as inbox --context.
	if flag := cmd.Flags().Lookup("context"); flag != nil && flag.Changed {
		return false
	}
	return !isAgentFacingCommand(cmd)
}

func isAgentFacingCommand(cmd *cobra.Command) bool {
	fields := strings.Fields(cmd.CommandPath())
	if len(fields) == 0 {
		return false
	}
	path := strings.Join(fields[1:], " ")
	for _, agentFacing := range agentFacingCommands {
		if path == agentFacing || strings.HasPrefix(path, agentFacing+" ") {
			return true
		}
	}
	return false
}

func reportsAgentUse(cmd *cobra.Command) bool {
	path := strings.Join(strings.Fields(cmd.CommandPath())[1:], " ")
	switch path {
	case "agent-hook contract", "agent-contract-hook":
		return true
	case "mcp serve":
		return false
	}
	return cliUseTarget.Load() != nil && (isAgentFacingCommand(cmd) || os.Getenv(hooks.HookVersionEnv) != "")
}

// reportCLIUse discovers an existing target only for contract hooks.
func reportCLIUse(cmd *cobra.Command) {
	agent := reportsAgentUse(cmd)
	target := cliUseTarget.Load()
	if !agent && (target == nil || !reportsCLIUse(cmd)) {
		return
	}
	now := cliUseNow().UTC()
	day := now.Format(time.DateOnly)
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	var sends posthog.SendGroup
	sends.Go(func() {
		target := target
		// Detached so a command that ends by cancelling its own context still reports.
		timeout := cliUseReportTimeout
		if agent {
			timeout = agentUseReportTimeout
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
		defer cancel()
		if target == nil {
			resolved, err := discoverDaemonResolved(ctx)
			if err != nil {
				return
			}
			target = &resolved
		}
		marker, finish, report := beginCLIUseReport(*target, agent, now)
		if !report {
			return
		}
		defer finish()
		hc, err := client.NewHTTPClientForResolved(ctx, *target, client.Opts{Timeout: timeout})
		if err != nil {
			return
		}
		apiClient, err := kataclient.NewWithHTTPClient(target.BaseURL, hc)
		if err != nil {
			return
		}
		body := &generated.CaptureTelemetryEventBody{Event: "app_opened", Properties: map[string]any{"surface": "cli"}}
		if agent {
			body = &generated.CaptureTelemetryEventBody{Event: "agent_active"}
		}
		resp, err := apiClient.CaptureTelemetryEventWithResponse(ctx, &generated.CaptureTelemetryEventRequestOptions{
			Body: body,
		})
		if err != nil {
			return
		}
		if agent && !agentUseDoneForDay(resp.JSON202.Status) {
			return
		}
		recordTelemetryDayMarker(marker, day)
	})
	sends.Wait(telemetry.ExitWait)
}

// beginCLIUseReport keeps successful days separate from unfinished CLI attempts.
func beginCLIUseReport(target client.ResolvedDaemon, agent bool, now time.Time) (string, func(), bool) {
	prefix := "cli-opened-"
	if agent {
		prefix = "agent-full-"
	}
	marker := telemetryDayMarkerPath(target, prefix)
	day := now.Format(time.DateOnly)
	if readTelemetryDayMarker(marker) == day {
		return "", nil, false
	}
	finish := func() {}
	if !agent {
		attempt := telemetryDayMarkerPath(target, "cli-opened-try-")
		previous, err := time.Parse(time.RFC3339, readTelemetryDayMarker(attempt))
		if err == nil && previous.UTC().Format(time.DateOnly) == day && now.Sub(previous) < cliUseRetryBackoff {
			return "", nil, false
		}
		recordTelemetryDayMarker(attempt, now.Format(time.RFC3339))
		finish = func() { _ = os.Remove(attempt) }
	}
	return marker, finish, true
}

// agentUseDoneForDay reports whether a receipt ends agent reporting until the
// next UTC day. A full day cannot count further calls, and a daemon with
// telemetry turned off records none.
func agentUseDoneForDay(status generated.CaptureTelemetryEventResponseBodyStatus) bool {
	return status == generated.Full || status == generated.Disabled
}

func telemetryDayMarkerPath(target client.ResolvedDaemon, prefix string) string {
	home, err := config.KataHome()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(target.BaseURL + "\n" + target.UnixSocket))
	return filepath.Join(home, "telemetry", prefix+hex.EncodeToString(sum[:8]))
}

func readTelemetryDayMarker(path string) string {
	if path == "" {
		return ""
	}
	day, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from KATA_HOME, not user input.
	if err != nil {
		return ""
	}
	return string(day)
}

// recordTelemetryDayMarker is best effort: without the marker the next command reports again.
// It skips fsync because a lost marker costs only one more report, while a sync would delay
// the command's exit.
func recordTelemetryDayMarker(path, day string) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = atomicfile.WriteFile(path, []byte(day), atomicfile.WithPerm(0o600), atomicfile.WithoutSync())
}
