package main

import (
	"strconv"
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
	var workspace, project, interval, since string
	var titlePrefix bool
	cmd := &cobra.Command{Use: "enable", Short: "enable daemon-side Plane sync for this project", Args: cobra.NoArgs}
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
		if cmd.Flags().Changed("interval") {
			value := strings.TrimSpace(interval)
			seconds, err := strconv.Atoi(value)
			if err != nil {
				duration, parseErr := time.ParseDuration(value)
				if parseErr != nil || duration < time.Second {
					return invalid("Plane sync interval must be at least one second")
				}
			} else if seconds < 1 {
				return invalid("Plane sync interval must be at least one second")
			}
			body.Interval = &value
		}
		a, projectID, err := githubSyncProjectAPI(cmd.Context())
		if err != nil {
			return err
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
	return cmd
}
