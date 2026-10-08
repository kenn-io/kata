package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/tickticksync"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newTickTickSyncCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "ticktick", Short: "mirror TickTick tasks into this project"}
	cmd.AddCommand(newTickTickSyncEnableCmd(), newIssueSyncDisableCmd("ticktick"), newIssueSyncStatusCmd("ticktick"), newIssueSyncOnceCmd("ticktick"))
	return cmd
}

func newTickTickSyncEnableCmd() *cobra.Command {
	var project, interval, statusSync string
	var titlePrefix bool
	cmd := &cobra.Command{Use: "enable", Short: "enable daemon-side TickTick sync for this project", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		config := map[string]any{}
		invalid := func(message string) error {
			return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
		}
		if cmd.Flags().Changed("ticktick-project") {
			if err := tickticksync.ValidateID(project); err != nil {
				return invalid(err.Error())
			}
			config["project_id"] = project
		}
		if cmd.Flags().Changed("title-prefix") {
			config["title_prefix"] = titlePrefix
		}
		body := &generated.EnableIssueSyncBody{Config: config}
		statusFlagsChanged := false
		if cmd.Flags().Changed("status-sync") {
			var err error
			body.StatusSync, err = issueStatusSyncMode(statusSync)
			if err != nil {
				return err
			}
			statusFlagsChanged = true
		}
		if cmd.Flags().Changed("interval") {
			value := strings.TrimSpace(interval)
			seconds, err := strconv.Atoi(value)
			if err != nil {
				duration, parseErr := time.ParseDuration(value)
				if parseErr != nil || duration < time.Second {
					return invalid("TickTick sync interval must be at least one second")
				}
			} else if seconds < 1 {
				return invalid("TickTick sync interval must be at least one second")
			}
			body.Interval = &value
		}
		a, projectID, err := githubSyncProjectAPI(cmd.Context())
		if err != nil {
			return err
		}
		if statusFlagsChanged {
			if err := requireIssueStatusSync(a); err != nil {
				return err
			}
		}
		a.client, err = longRunningClientForResolved(cmd.Context(), a.resolved)
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		response, callErr := client.EnableIssueSyncWithResponse(a.ctx, &generated.EnableIssueSyncRequestOptions{PathParams: &generated.EnableIssueSyncPath{ProjectID: projectID, Provider: "ticktick"}, Body: body})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		return issueSyncPrintBinding(cmd.OutOrStdout(), response.Body, "ticktick", "enabled")
	}
	cmd.Flags().StringVar(&project, "ticktick-project", "", "TickTick project ID (required initially; --project selects the Kata project)")
	cmd.Flags().StringVar(&interval, "interval", "", "poll duration or seconds (initial default: 5m; re-enable preserves saved interval)")
	cmd.Flags().BoolVar(&titlePrefix, "title-prefix", true, "prefix titles with [TickTick]; false keeps source titles and adds the ticktick label (omitted preserves saved choice)")
	cmd.Flags().StringVar(&statusSync, "status-sync", "", "status direction: one-way or two-way (omitted preserves saved mode; new bindings default to one-way)")
	return cmd
}
