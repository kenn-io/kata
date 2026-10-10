package main

import (
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/twentysync"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newTwentySyncCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "twenty", Short: "mirror Twenty tasks into this project"}
	cmd.AddCommand(newTwentySyncEnableCmd(), newIssueSyncDisableCmd("twenty"), newIssueSyncStatusCmd("twenty"), newIssueSyncOnceCmd("twenty"))
	return cmd
}

func newTwentySyncEnableCmd() *cobra.Command {
	var interval, since, statusSync, closedStatus, openStatus string
	var openStatuses []string
	var titlePrefix bool
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "enable daemon-side Twenty sync for this project",
		Long: `Bind the Kata project (bound or --project) to the built-in tasks of the
Twenty workspace that owns the daemon's API key, and start daemon-side
polling. There is no source workspace flag. The daemon reads its key from
KATA_TWENTY_TOKEN; [twenty_sync] in config.toml sets token_env and
self-hosted origins. Re-enable keeps every option you omit. --since limits
the import to tasks updated after a date; pass --since= to clear it.
--status-sync two-way also sends Kata close and reopen back to Twenty.
Status flags take API option values, not display labels. Defaults:
--closed-status DONE, --open-status TODO, --open-statuses TODO,IN_PROGRESS.`,
		Example: `  kata sync twenty enable --agent
  kata sync twenty enable --status-sync two-way --closed-status COMPLETE --open-status READY --open-statuses READY,ACTIVE --agent`,
		Args: cobra.NoArgs,
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		config := map[string]any{}
		invalid := func(message string) error {
			return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
		}
		if cmd.Flags().Changed("since") {
			cutoff, err := twentysync.ParseSince(since)
			if err != nil {
				return invalid(err.Error())
			}
			config["since"] = ""
			if cutoff != nil {
				config["since"] = cutoff.Format(time.RFC3339)
			}
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
		for _, target := range []struct{ flag, key, value string }{{"closed-status", "closed_status", closedStatus}, {"open-status", "open_status", openStatus}} {
			if !cmd.Flags().Changed(target.flag) {
				continue
			}
			if !twentysync.ValidStatusValue(target.value) {
				return invalid("Twenty status options must be nonempty API values")
			}
			config[target.key] = target.value
			statusFlagsChanged = true
		}
		if cmd.Flags().Changed("open-statuses") {
			if len(openStatuses) == 0 {
				return invalid("Twenty open statuses must not be empty")
			}
			seen := map[string]bool{}
			for _, value := range openStatuses {
				if !twentysync.ValidStatusValue(value) || seen[value] {
					return invalid("Twenty open statuses must be distinct nonempty API values")
				}
				seen[value] = true
			}
			config["open_statuses"] = openStatuses
			statusFlagsChanged = true
		}
		if cmd.Flags().Changed("interval") {
			value, err := issueSyncIntervalFlag("twenty", interval)
			if err != nil {
				return err
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
		response, callErr := client.EnableIssueSyncWithResponse(a.ctx, &generated.EnableIssueSyncRequestOptions{PathParams: &generated.EnableIssueSyncPath{ProjectID: projectID, Provider: "twenty"}, Body: body})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		return issueSyncPrintBinding(cmd.OutOrStdout(), response.Body, "twenty", "enabled")
	}
	cmd.Flags().StringVar(&interval, "interval", "", "poll duration or seconds (initial default: 5m; re-enable preserves saved interval)")
	cmd.Flags().BoolVar(&titlePrefix, "title-prefix", true, "prefix titles with [Twenty task-ID]; false keeps source titles and adds the twenty label (omitted preserves saved choice)")
	cmd.Flags().StringVar(&since, "since", "", "updated-after UTC date or whole-second RFC3339; empty clears, omitted preserves saved cutoff")
	cmd.Flags().StringVar(&statusSync, "status-sync", "", "status direction: one-way or two-way (omitted preserves saved mode; new bindings default to one-way)")
	cmd.Flags().StringVar(&closedStatus, "closed-status", "", "closed API status value (initial default: DONE; omitted preserves saved mapping)")
	cmd.Flags().StringVar(&openStatus, "open-status", "", "reopen API status value, classified open (initial default: TODO)")
	cmd.Flags().StringSliceVar(&openStatuses, "open-statuses", nil, "comma-separated API statuses classified open (initial defaults: TODO,IN_PROGRESS)")
	return cmd
}
