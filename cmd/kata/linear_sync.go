package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/linearsync"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newLinearSyncCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "linear", Short: "mirror Linear tasks into this project"}
	cmd.AddCommand(newLinearSyncEnableCmd(), newIssueSyncDisableCmd("linear"), newIssueSyncStatusCmd("linear"), newIssueSyncOnceCmd("linear"))
	return cmd
}

func newLinearSyncEnableCmd() *cobra.Command {
	var workspace, team, project, interval, since, statusSync, closedState, openState string
	var titlePrefix bool
	cmd := &cobra.Command{Use: "enable", Short: "enable daemon-side Linear sync for this project",
		Long: `Bind the Kata project (bound or --project) to one Linear team and start
daemon-side polling. The first enable needs --linear-workspace and
--linear-team (UUIDs); --linear-project narrows it to one Linear project.
Re-enable keeps every option you omit. --status-sync two-way also writes
open and closed states back; --open-state and --closed-state pick the target
states.`,
		Example: `  kata sync linear enable --linear-workspace <workspace-uuid> --linear-team <team-uuid> --agent
  kata sync linear enable --status-sync two-way --interval 10m --agent`, Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		config := map[string]any{}
		invalid := func(message string) error {
			return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
		}
		for _, scope := range []struct{ flag, key, value string }{{"linear-workspace", "workspace_id", workspace}, {"linear-team", "team_id", team}} {
			if cmd.Flags().Changed(scope.flag) {
				id, err := linearsync.CanonicalID(scope.value)
				if err != nil {
					return invalid(err.Error())
				}
				config[scope.key] = id
			}
		}
		if cmd.Flags().Changed("linear-project") {
			id, err := linearsync.CanonicalID(project)
			if err != nil {
				return invalid(err.Error())
			}
			config["project_id"] = id
		}
		if cmd.Flags().Changed("since") {
			cutoff, err := linearsync.ParseSince(since)
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
		for _, target := range []struct{ flag, key, value string }{{"closed-state", "closed_state_id", closedState}, {"open-state", "open_state_id", openState}} {
			if !cmd.Flags().Changed(target.flag) {
				continue
			}
			statusFlagsChanged = true
			value := strings.TrimSpace(target.value)
			if value != "" {
				var err error
				value, err = linearsync.CanonicalID(value)
				if err != nil {
					return invalid(err.Error())
				}
			}
			config[target.key] = value
		}
		if cmd.Flags().Changed("interval") {
			value := strings.TrimSpace(interval)
			seconds, err := strconv.Atoi(value)
			if err != nil {
				duration, parseErr := time.ParseDuration(value)
				if parseErr != nil || duration < time.Second {
					return invalid("Linear sync interval must be at least one second")
				}
			} else if seconds < 1 {
				return invalid("Linear sync interval must be at least one second")
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
		response, callErr := client.EnableIssueSyncWithResponse(a.ctx, &generated.EnableIssueSyncRequestOptions{PathParams: &generated.EnableIssueSyncPath{ProjectID: projectID, Provider: "linear"}, Body: body})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		return issueSyncPrintBinding(cmd.OutOrStdout(), response.Body, "linear", "enabled")
	}
	cmd.Flags().StringVar(&workspace, "linear-workspace", "", "Linear workspace UUID (required initially)")
	cmd.Flags().StringVar(&team, "linear-team", "", "Linear team UUID (required initially)")
	cmd.Flags().StringVar(&project, "linear-project", "", "optional Linear project UUID (set initially; --project selects the Kata project)")
	cmd.Flags().StringVar(&interval, "interval", "", "poll duration or seconds (initial default: 5m; re-enable preserves saved interval)")
	cmd.Flags().BoolVar(&titlePrefix, "title-prefix", true, "prefix titles with [Linear IDENTIFIER-N]; false keeps source titles and adds the linear label (omitted preserves saved choice)")
	cmd.Flags().StringVar(&since, "since", "", "updated-after UTC date or whole-second RFC3339; empty clears, omitted preserves saved cutoff")
	cmd.Flags().StringVar(&statusSync, "status-sync", "", "status direction: one-way or two-way (omitted preserves saved mode; new bindings default to one-way)")
	cmd.Flags().StringVar(&closedState, "closed-state", "", "closed write target UUID in the completed group; empty clears override to live group default")
	cmd.Flags().StringVar(&openState, "open-state", "", "open write target UUID in the unstarted group; empty clears override to live group default")
	return cmd
}
