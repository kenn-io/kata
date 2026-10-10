package main

import (
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/notionsync"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newNotionSyncCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "notion", Short: "mirror Notion tasks into this project"}
	cmd.AddCommand(newNotionSyncEnableCmd(), newIssueSyncDisableCmd("notion"), newIssueSyncStatusCmd("notion"), newIssueSyncOnceCmd("notion"))
	return cmd
}

func newNotionSyncEnableCmd() *cobra.Command {
	var source, database, status, assignee, interval, since, statusSync, completeGroup, todoGroup, closedStatus, openStatus string
	var done []string
	var titlePrefix bool
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "enable daemon-side Notion sync for this project",
		Long: `Bind the Kata project (bound or --project) to one Notion task data source
and start daemon-side polling. The first enable needs one locator:
--data-source (UUID), or --database (UUID or URL of a database with exactly
one data source). The source needs a People property; --assignee-property may
be omitted only when exactly one exists. --status-property defaults to the
sole status property. Selectors take exact IDs or case-sensitive names.
The daemon reads its token from KATA_NOTION_TOKEN, or the variable named by
[notion_sync] token_env. Re-enable keeps every option you omit; the data
source and selected properties cannot change. Pass --since= to clear the
cutoff. --status-sync two-way also sends Kata close and reopen back to the
status property. It needs a To-do group, rejects --done-status, and drops
saved done statuses.`,
		Example: `  kata sync notion enable --data-source <data-source-uuid> --agent
  kata sync notion enable --database <database-uuid> --assignee-property Responsible --agent
  kata sync notion enable --status-sync two-way --agent`,
		Args: cobra.NoArgs,
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		config := map[string]any{}
		invalid := func(message string) error {
			return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
		}
		if cmd.Flags().Changed("data-source") && cmd.Flags().Changed("database") {
			return invalid("provide exactly one Notion --data-source or --database")
		}
		for _, locator := range []struct{ flag, key, value string }{{"data-source", "data_source_id", source}, {"database", "database", database}} {
			if !cmd.Flags().Changed(locator.flag) {
				continue
			}
			if locator.flag == "data-source" && strings.ContainsAny(locator.value, "/:?#") {
				return invalid("Notion data source must be a UUID")
			}
			id, err := notionsync.ParseDatabaseLocator(locator.value)
			if err != nil {
				return invalid(err.Error())
			}
			config[locator.key] = id
		}
		for _, selector := range []struct{ flag, key, value string }{{"status-property", "status_property", status}, {"assignee-property", "assignee_property", assignee}} {
			if cmd.Flags().Changed(selector.flag) {
				if strings.TrimSpace(selector.value) == "" {
					return invalid("Notion " + selector.flag + " must be nonempty")
				}
				config[selector.key] = selector.value
			}
		}
		if cmd.Flags().Changed("done-status") {
			for _, value := range done {
				if strings.TrimSpace(value) == "" {
					return invalid("Notion --done-status must be nonempty")
				}
			}
			config["done_statuses"] = done
		}
		if cmd.Flags().Changed("since") {
			cutoff, err := notionsync.ParseSince(since)
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
		if cmd.Flags().Changed("status-sync") {
			var err error
			body.StatusSync, err = issueStatusSyncMode(statusSync)
			if err != nil {
				return err
			}
			if statusSync == "two-way" && cmd.Flags().Changed("done-status") {
				return invalid("Notion --done-status cannot be combined with two-way status sync")
			}
		}
		for _, selector := range []struct{ flag, key, value string }{{"complete-group", "complete_group", completeGroup}, {"todo-group", "todo_group", todoGroup}, {"closed-status", "closed_status", closedStatus}, {"open-status", "open_status", openStatus}} {
			if !cmd.Flags().Changed(selector.flag) {
				continue
			}
			if (selector.flag == "complete-group" || selector.flag == "todo-group") && strings.TrimSpace(selector.value) == "" {
				return invalid("Notion --" + selector.flag + " must be nonempty")
			}
			config[selector.key] = selector.value
		}
		if cmd.Flags().Changed("interval") {
			value, err := issueSyncIntervalFlag("notion", interval)
			if err != nil {
				return err
			}
			body.Interval = &value
		}
		a, projectID, err := githubSyncProjectAPI(cmd.Context())
		if err != nil {
			return err
		}
		if notionStatusSyncFlagsChanged(cmd) {
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
		response, callErr := client.EnableIssueSyncWithResponse(a.ctx, &generated.EnableIssueSyncRequestOptions{PathParams: &generated.EnableIssueSyncPath{ProjectID: projectID, Provider: "notion"}, Body: body})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		return issueSyncPrintBinding(cmd.OutOrStdout(), response.Body, "notion", "enabled")
	}
	cmd.Flags().StringVar(&statusSync, "status-sync", "", "status direction: one-way or two-way (omitted preserves saved mode; new bindings default to one-way)")
	cmd.Flags().StringVar(&completeGroup, "complete-group", "", "Complete workflow group ID or exact name (default: Complete)")
	cmd.Flags().StringVar(&todoGroup, "todo-group", "", "To-do workflow group ID or exact name (default: To-do)")
	cmd.Flags().StringVar(&closedStatus, "closed-status", "", "closed write target ID or exact name; empty clears override to live Complete group default")
	cmd.Flags().StringVar(&openStatus, "open-status", "", "open write target ID or exact name; empty clears override to live To-do group default")
	cmd.Flags().StringVar(&source, "data-source", "", "Notion data source UUID (initial enable requires a locator)")
	cmd.Flags().StringVar(&database, "database", "", "Notion database UUID or URL; must contain exactly one data source")
	cmd.Flags().StringVar(&status, "status-property", "", "status property ID or exact case-sensitive name (default: sole status property)")
	cmd.Flags().StringVar(&assignee, "assignee-property", "", "required People property: ID or exact case-sensitive name (selector may be omitted when exactly one exists)")
	cmd.Flags().StringArrayVar(&done, "done-status", nil, "completed status option ID or exact name; repeat for each option (legacy one-way completion mapping)")
	cmd.Flags().StringVar(&interval, "interval", "", "poll duration or seconds (initial default: 5m; re-enable preserves saved interval)")
	cmd.Flags().BoolVar(&titlePrefix, "title-prefix", true, "prefix titles with [Notion]; false keeps source titles and adds the notion label (omitted preserves saved choice)")
	cmd.Flags().StringVar(&since, "since", "", "updated-after UTC date or whole-second RFC3339; empty clears, omitted preserves saved cutoff")
	return cmd
}
