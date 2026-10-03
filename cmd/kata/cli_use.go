package main

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/hooks"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// cliUseReportTimeout bounds the use report on the command's exit path; var so tests can shorten it.
var cliUseReportTimeout = time.Second

// cliUseTarget is the resolved daemon this invocation built its own client from.
var cliUseTarget atomic.Pointer[client.ResolvedDaemon]

// agentFacingCommands run for agents or hook runners, not a person at a prompt.
var agentFacingCommands = []string{"mcp serve", "agent-hooks", "attention-hook", "agent-contract-hook"}

func recordCLIUseTarget(resolved client.ResolvedDaemon) {
	cliUseTarget.Store(&resolved)
}

// reportsCLIUse reports whether a successful cmd counts as a person's use.
func reportsCLIUse(cmd *cobra.Command) bool {
	if mode := currentOutputMode(); mode == outputAgent || mode == outputContract {
		return false
	}
	if os.Getenv(hooks.HookVersionEnv) != "" {
		return false
	}
	fields := strings.Fields(cmd.CommandPath())
	if len(fields) == 0 {
		return true
	}
	path := strings.Join(fields[1:], " ")
	for _, agentFacing := range agentFacingCommands {
		if path == agentFacing || strings.HasPrefix(path, agentFacing+" ") {
			return false
		}
	}
	return true
}

// reportCLIUse sends app_opened with surface cli to the daemon the command used.
// It never resolves or starts a daemon, and drops every outcome.
func reportCLIUse(cmd *cobra.Command) {
	target := cliUseTarget.Load()
	if target == nil || !reportsCLIUse(cmd) {
		return
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	// Detached so a command that ends by cancelling its own context still reports.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cliUseReportTimeout)
	defer cancel()
	hc, err := client.NewHTTPClientForResolved(ctx, *target, client.Opts{Timeout: cliUseReportTimeout})
	if err != nil {
		return
	}
	apiClient, err := kataclient.NewWithHTTPClient(target.BaseURL, hc)
	if err != nil {
		return
	}
	_, _ = apiClient.CaptureTelemetryEventWithResponse(ctx, &generated.CaptureTelemetryEventRequestOptions{
		Body: &generated.CaptureTelemetryEventBody{Event: "app_opened", Properties: map[string]any{"surface": "cli"}},
	})
}
