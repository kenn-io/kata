package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/version"
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
	if !posthog.Eligible("KATA", version.Version) {
		return false
	}
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
	if agent && !posthog.Eligible("KATA", version.Version) {
		return
	}
	if !agent && (target == nil || !reportsCLIUse(cmd)) {
		return
	}
	body := &generated.CaptureTelemetryEventBody{Event: "app_opened", Properties: map[string]any{"surface": "cli"}}
	if agent {
		body = &generated.CaptureTelemetryEventBody{Event: "agent_active"}
	}
	sendCLIEvent(cmd, body, agent, "")
}

func reportCLIOutcome(cmd *cobra.Command, event, kind string) {
	if reportsCLIOutcome(cmd) {
		sendCLIEvent(cmd, cliOutcomeBody(event, kind), false, "")
	}
}

// reportCLIEmptySearch remembers only the hash of the last empty query for this daemon.
func reportCLIEmptySearch(cmd *cobra.Command, query string) {
	if reportsCLIOutcome(cmd) {
		sum := sha256.Sum256([]byte(strings.TrimSpace(query)))
		sendCLIEvent(cmd, cliOutcomeBody("search_empty", ""), false, hex.EncodeToString(sum[:]))
	}
}

// reportsCLIOutcome reports whether a person reads this command's output.
func reportsCLIOutcome(cmd *cobra.Command) bool {
	return currentOutputMode() == outputHuman && reportsCLIUse(cmd)
}

func cliOutcomeBody(event, kind string) *generated.CaptureTelemetryEventBody {
	properties := map[string]any{"surface": "cli"}
	if kind != "" {
		properties["kind"] = kind
	}
	return &generated.CaptureTelemetryEventBody{Event: event, Properties: properties}
}

// sendCLIEvent claims search when set, the hash of an empty query, so a
// repeat of the last empty search sends nothing once a receipt accepts it.
func sendCLIEvent(cmd *cobra.Command, body *generated.CaptureTelemetryEventBody, agent bool, search string) {
	target := cliUseTarget.Load()
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
			key := client.InspectTelemetryTarget(ctx, workspaceStartForRemote(), flags.Daemon)
			if key != "" && client.ReadTelemetryDayMarker(client.TelemetryDisabledMarkerPath(key)) == day {
				return
			}
			resolved, err := discoverDaemonResolved(ctx)
			if err != nil {
				return
			}
			target = &resolved
		}
		claim, mark, finish, report := beginCLIClaim(*target, body.Event, agent, search, now)
		if !report {
			return
		}
		defer finish()
		var gate client.TelemetryGate
		_, _ = gate.ReportClaim(ctx, client.TelemetryTargetKey(*target), claim, mark, day, agent, func(ctx context.Context) (*generated.CaptureTelemetryEventResponse, error) {
			hc, err := client.NewHTTPClientForResolved(ctx, *target, client.Opts{Timeout: timeout})
			if err != nil {
				return nil, err
			}
			apiClient, err := kataclient.NewWithHTTPClient(target.BaseURL, hc)
			if err != nil {
				return nil, err
			}
			return apiClient.CaptureTelemetryEvent(ctx, &generated.CaptureTelemetryEventRequestOptions{Body: body})
		})
	})
	sends.Wait(telemetry.ExitWait)
}

// beginCLIClaim picks the marker that ends repeat reports of event.
func beginCLIClaim(target client.ResolvedDaemon, event string, agent bool, search string, now time.Time) (claim, mark string, finish func(), report bool) {
	if search != "" {
		return client.TelemetryDayMarkerPathForKey(client.TelemetryTargetKey(target), "cli-search-empty-"), search, func() {}, true
	}
	mark = now.Format(time.DateOnly)
	if event != "app_opened" && !agent {
		return "", mark, func() {}, true
	}
	claim, finish, report = beginCLIUseReport(target, agent, now)
	return claim, mark, finish, report
}

// beginCLIUseReport keeps successful days separate from unfinished CLI attempts.
func beginCLIUseReport(target client.ResolvedDaemon, agent bool, now time.Time) (string, func(), bool) {
	prefix := "cli-opened-"
	if agent {
		prefix = "agent-full-"
	}
	claim := client.TelemetryDayMarkerPath(target, prefix)
	day := now.Format(time.DateOnly)
	if client.ReadTelemetryDayMarker(claim) == day {
		return "", nil, false
	}
	finish := func() {}
	if !agent {
		attempt := client.TelemetryDayMarkerPath(target, "cli-opened-try-")
		previous, err := time.Parse(time.RFC3339, client.ReadTelemetryDayMarker(attempt))
		if err == nil && previous.UTC().Format(time.DateOnly) == day && now.Sub(previous) < cliUseRetryBackoff {
			return "", nil, false
		}
		client.RecordTelemetryDayMarker(attempt, now.Format(time.RFC3339))
		finish = func() { _ = os.Remove(attempt) }
	}
	return claim, finish, true
}
