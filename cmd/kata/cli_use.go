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

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/hooks"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// cliUseReportTimeout bounds the use report on the command's exit path; var so tests can shorten it.
var cliUseReportTimeout = time.Second

// agentUseReportTimeout keeps exit reports within installed hook deadlines.
var agentUseReportTimeout = 100 * time.Millisecond

// cliUseNow picks the UTC day a report counts for; var so tests can move it.
var cliUseNow = time.Now

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
	day := cliUseNow().UTC().Format(time.DateOnly)
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	// Detached so a command that ends by cancelling its own context still reports.
	timeout := cliUseReportTimeout
	if agent {
		timeout = agentUseReportTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	if target == nil {
		resolved, err := discoverDaemonResolved(ctx)
		if err != nil {
			return
		}
		target = &resolved
	}
	marker := ""
	if !agent {
		marker = cliUseMarkerPath(*target)
		if readCLIUseDay(marker) == day {
			return
		}
	}
	body := &generated.CaptureTelemetryEventBody{Event: "app_opened", Properties: map[string]any{"surface": "cli"}}
	if agent {
		body = &generated.CaptureTelemetryEventBody{Event: "agent_active"}
	}
	if captureCLIUsage(ctx, *target, timeout, body) && !agent {
		recordCLIUseDay(marker, day)
	}
}

func captureCLIUsage(ctx context.Context, target client.ResolvedDaemon, timeout time.Duration, body *generated.CaptureTelemetryEventBody) bool {
	hc, err := client.NewHTTPClientForResolved(ctx, target, client.Opts{Timeout: timeout})
	if err != nil {
		return false
	}
	apiClient, err := kataclient.NewWithHTTPClient(target.BaseURL, hc)
	if err != nil {
		return false
	}
	resp, err := apiClient.CaptureTelemetryEventWithResponse(ctx, &generated.CaptureTelemetryEventRequestOptions{Body: body})
	return err == nil && resp != nil && resp.JSON202 != nil
}

// reportCLIIssueAction starts delivery at commit acknowledgement. Its returned
// join runs after command output and follow-ups, including failed comments.
func reportCLIIssueAction(cmd *cobra.Command, event string, changed bool) func() {
	target := cliUseTarget.Load()
	if !changed || target == nil || !reportsCLIUse(cmd) {
		return func() {}
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cliUseReportTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		captureCLIUsage(ctx, *target, cliUseReportTimeout, &generated.CaptureTelemetryEventBody{
			Event: event, Properties: map[string]any{"surface": "cli"},
		})
	}()
	return func() { <-done }
}

// cliUseMarkerPath names the file holding the last UTC day target accepted a
// report from this KATA_HOME, or "" when there is no home to keep it in.
func cliUseMarkerPath(target client.ResolvedDaemon) string {
	home, err := config.KataHome()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(target.BaseURL + "\n" + target.UnixSocket))
	return filepath.Join(home, "telemetry", "cli-opened-"+hex.EncodeToString(sum[:8]))
}

func readCLIUseDay(path string) string {
	if path == "" {
		return ""
	}
	day, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from KATA_HOME, not user input.
	if err != nil {
		return ""
	}
	return string(day)
}

// recordCLIUseDay is best effort: without the marker the next command reports again.
func recordCLIUseDay(path, day string) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(day), 0o600)
}
