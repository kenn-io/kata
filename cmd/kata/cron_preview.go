package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newCronPreviewCmd() *cobra.Command {
	options := cronCLIOptions{resource: "job", action: "create"}
	count := 10
	var from string
	command := &cobra.Command{
		Use:   "preview [job-uid]",
		Short: "preview a job's occurrences without changing delivery",
		Long: `List when a native recurring job would create children, in UTC and the
trigger's timezone, without saving or changing anything. For a stored job,
also show the first batch the planner would deliver from its current progress.
That is neither a full sweep nor a delivery-time guarantee.
For an adapter job, show only the trigger's occurrences; the adapter decides
overlap, catch-up and activation. Results are JSON.`,
		Example: `  kata cron job preview <job-uid> --count 10 --json
  kata cron job preview --file job.json --parent example-project#abc4 --json`,
		Args: cobra.MaximumNArgs(1),
	}
	command.Flags().StringVar(&options.file, "file", "", "job request JSON file, or - for stdin")
	command.Flags().StringVar(&options.parent, "parent", "", "parent issue ref for an unsaved per-run job")
	command.Flags().IntVar(&count, "count", 10, "number of occurrences (1-100)")
	command.Flags().StringVar(&from, "from", "", "RFC3339 starting instant (default: daemon clock)")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		hasFile := cmd.Flags().Changed("file")
		if len(args) == 1 && hasFile {
			return cronCLIValidation("pass a job UID or --file, not both")
		}
		if len(args) == 0 && !hasFile {
			return cronCLIValidation("pass a job UID or --file; use --file - for stdin")
		}
		if !hasFile && cmd.Flags().Changed("parent") {
			return cronCLIValidation("--parent applies only to --file")
		}
		if count < 1 || count > 100 {
			return cronCLIValidation("count must be between 1 and 100")
		}
		if cmd.Flags().Changed("from") {
			parsed, err := time.Parse(time.RFC3339, from)
			if err != nil || parsed.IsZero() || parsed.UTC().Year() < 0 || parsed.UTC().Year() > 9999 {
				return cronCLIValidation("--from must be an RFC3339 instant")
			}
		}
		if cmd.Flags().Changed("parent") && strings.TrimSpace(options.parent) == "" {
			return cronCLIValidation("--parent must not be empty")
		}
		var raw []byte
		var err error
		if hasFile {
			raw, err = options.readRequest(cmd)
			if err != nil {
				return err
			}
			if _, err = options.decodeRequest(raw); err != nil {
				return err
			}
		} else {
			options.identity, err = normalizedCronCLIUID(args[0])
			if err != nil {
				return err
			}
		}
		daemon, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		// Check before project or parent reads so old daemons fail with upgrade advice.
		if err := requireDaemonAPIVersion(daemon.ctx, daemon.client, daemon.baseURL, apiVersionCronPreview, "kata cron job preview"); err != nil {
			return err
		}
		var project projectRef
		if options.parent != "" {
			project, raw, err = options.applyParent(cmd, daemon, raw)
		} else if strings.TrimSpace(flags.Project) != "" {
			project, err = resolveProjectSelectorIncludingArchived(daemon, flags.Project)
		} else {
			project, err = resolveProjectArgFlagOrWorkspace(daemon, nil)
		}
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(daemon.baseURL, daemon.client)
		if err != nil {
			return err
		}
		if hasFile {
			var request api.PutCronJobBody
			if err := json.Unmarshal(raw, &request); err != nil {
				return err
			}
			// Name, actor and replacement preconditions are accepted from create files
			// but do not participate in a read-only definition preview.
			body, err := json.Marshal(api.PreviewCronDefinitionBody{Definition: request.Definition, Count: &count, From: from})
			if err != nil {
				return err
			}
			var generatedBody generated.PreviewCronDefinitionBody
			if err := decodeCronCLIRequest(body, &generatedBody); err != nil {
				return err
			}
			response, callErr := client.PreviewCronDefinitionWithResponse(daemon.ctx, &generated.PreviewCronDefinitionRequestOptions{PathParams: &generated.PreviewCronDefinitionPath{ProjectID: project.ID}, Body: &generatedBody})
			if response == nil {
				return externalCLITransportError(response, callErr)
			}
			previewRaw, responseErr := cronCLIResponse(response.StatusCode, response.Body, callErr)
			if responseErr != nil {
				return responseErr
			}
			raw = previewRaw
		} else {
			textCount := strconv.Itoa(count)
			query := &generated.PreviewCronJobQuery{Count: &textCount}
			if from != "" {
				query.From = &from
			}
			response, callErr := client.PreviewCronJobWithResponse(daemon.ctx, &generated.PreviewCronJobRequestOptions{PathParams: &generated.PreviewCronJobPath{ProjectID: project.ID, CronUID: options.identity}, Query: query})
			if response == nil {
				return externalCLITransportError(response, callErr)
			}
			previewRaw, responseErr := cronCLIResponse(response.StatusCode, response.Body, callErr)
			if responseErr != nil {
				return responseErr
			}
			raw = previewRaw
		}
		return emitJSON(cmd.OutOrStdout(), jsontext.Value(raw))
	}
	return command
}
