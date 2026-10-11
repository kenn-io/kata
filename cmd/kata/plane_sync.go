package main

import (
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/planesync"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newPlaneSyncCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "plane", Short: "mirror Plane tasks into this project"}
	cmd.AddCommand(newPlaneSyncEnableCmd(), newIssueSyncDisableCmd("plane"), newIssueSyncStatusCmd("plane"), newIssueSyncOnceCmd("plane"))
	return cmd
}

func newPlaneSyncEnableCmd() *cobra.Command {
	var workspace, project, interval, since, statusSync, closedState, openState string
	var titlePrefix bool
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "enable daemon-side Plane sync for this project",
		Long: `Bind the Kata project (bound or --project) to one Plane project and start
daemon-side polling. The first enable needs --plane-workspace (slug) and
--plane-project (UUID); both are fixed after that. The daemon reads its API
key from KATA_PLANE_TOKEN; [plane_sync] in config.toml sets token_env and
self-hosted origins. Re-enable keeps every option you omit. --since limits
the import to work items updated after a date; pass --since= to clear it.
--status-sync two-way also sends Kata close and reopen back to Plane;
--closed-state and --open-state pick the target states.`,
		Example: `  kata sync plane enable --plane-workspace example-workspace --plane-project <project-uuid> --agent
  kata sync plane enable --status-sync two-way --interval 10m --agent`,
		Args: cobra.NoArgs,
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		config := map[string]any{}
		invalid := func(message string) error {
			return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
		}
		if cmd.Flags().Changed("plane-workspace") {
			if err := planesync.ValidateWorkspace(workspace); err != nil {
				return invalid(err.Error())
			}
			config["workspace"] = workspace
		}
		if cmd.Flags().Changed("plane-project") {
			id, err := planesync.CanonicalID(project)
			if err != nil {
				return invalid(err.Error())
			}
			config["project_id"] = id
		}
		if cmd.Flags().Changed("since") {
			cutoff, err := planesync.ParseSince(since)
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
				value, err = planesync.CanonicalID(value)
				if err != nil {
					return invalid(err.Error())
				}
			}
			config[target.key] = value
		}
		if cmd.Flags().Changed("interval") {
			value, err := issueSyncIntervalFlag("plane", interval)
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
		response, callErr := client.EnableIssueSyncWithResponse(a.ctx, &generated.EnableIssueSyncRequestOptions{PathParams: &generated.EnableIssueSyncPath{ProjectID: projectID, Provider: "plane"}, Body: body})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		return issueSyncPrintBinding(cmd.OutOrStdout(), response.Body, "plane", "enabled")
	}
	cmd.Flags().StringVar(&workspace, "plane-workspace", "", "Plane workspace slug (required initially)")
	cmd.Flags().StringVar(&project, "plane-project", "", "Plane project UUID (required initially; --project selects the Kata project)")
	cmd.Flags().StringVar(&interval, "interval", "", "poll duration or seconds (initial default: 5m; re-enable preserves saved interval)")
	cmd.Flags().BoolVar(&titlePrefix, "title-prefix", true, "prefix titles with [Plane IDENTIFIER-N]; false keeps source titles and adds the plane label (omitted preserves saved choice)")
	cmd.Flags().StringVar(&since, "since", "", "updated-after UTC date or whole-second RFC3339; empty clears, omitted preserves saved cutoff")
	cmd.Flags().StringVar(&statusSync, "status-sync", "", "status direction: one-way or two-way (omitted preserves saved mode; new bindings default to one-way)")
	cmd.Flags().StringVar(&closedState, "closed-state", "", "closed write target UUID in the completed group; empty clears override to live group default")
	cmd.Flags().StringVar(&openState, "open-state", "", "open write target UUID in the unstarted group; empty clears override to live group default")
	return cmd
}
