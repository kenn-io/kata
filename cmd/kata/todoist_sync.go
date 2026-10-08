package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/todoistsync"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newTodoistSyncCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "todoist", Short: "mirror Todoist tasks into this project"}
	cmd.AddCommand(newTodoistSyncEnableCmd(), newIssueSyncDisableCmd("todoist"), newIssueSyncStatusCmd("todoist"), newIssueSyncOnceCmd("todoist"))
	return cmd
}

func newTodoistSyncEnableCmd() *cobra.Command {
	var project, interval, since, statusSync string
	var titlePrefix bool
	cmd := &cobra.Command{Use: "enable", Short: "enable daemon-side Todoist sync for this project", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		config := map[string]any{}
		invalid := func(message string) error {
			return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
		}
		if cmd.Flags().Changed("todoist-project") {
			err := todoistsync.ValidateID(project)
			if err != nil {
				return invalid(err.Error())
			}
			config["project_id"] = project
		}
		if cmd.Flags().Changed("history-since") {
			cutoff, err := todoistsync.ParseHistorySince(since)
			if err != nil {
				return invalid(err.Error())
			}
			config["history_since"] = cutoff.Format(time.RFC3339)
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
					return invalid("Todoist sync interval must be at least one second")
				}
			} else if seconds < 1 {
				return invalid("Todoist sync interval must be at least one second")
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
		response, callErr := client.EnableIssueSyncWithResponse(a.ctx, &generated.EnableIssueSyncRequestOptions{PathParams: &generated.EnableIssueSyncPath{ProjectID: projectID, Provider: "todoist"}, Body: body})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		return issueSyncPrintBinding(cmd.OutOrStdout(), response.Body, "todoist", "enabled")
	}
	cmd.Flags().StringVar(&project, "todoist-project", "", "Todoist project opaque ID (required initially; --project selects the Kata project)")
	cmd.Flags().StringVar(&interval, "interval", "", "poll duration or seconds (initial default: 5m; re-enable preserves saved interval)")
	cmd.Flags().BoolVar(&titlePrefix, "title-prefix", true, "prefix titles with [Todoist]; false keeps source titles and adds the todoist label (omitted preserves saved choice)")
	cmd.Flags().StringVar(&since, "history-since", "", "completion history floor: UTC date or whole-second RFC3339 (initial default: 30 days ago; immutable)")
	cmd.Flags().StringVar(&statusSync, "status-sync", "", "status direction: one-way or two-way (omitted preserves saved mode; new bindings default to one-way)")
	return cmd
}
