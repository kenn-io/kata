package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cron"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronNextOptions struct {
	count int
	from  string
}

type cronNextOccurrence struct {
	At    time.Time `json:"at"`
	Local string    `json:"local"`
}

type cronNextResult struct {
	JobUID             string               `json:"job_uid"`
	DefinitionEventUID string               `json:"definition_event_uid"`
	ParentRevision     int64                `json:"parent_revision,omitzero"`
	AsOf               time.Time            `json:"as_of"`
	Basis              string               `json:"basis"`
	Timezone           string               `json:"timezone"`
	Delivery           string               `json:"delivery"`
	Status             string               `json:"status"`
	Occurrences        []cronNextOccurrence `json:"occurrences"`
	Warnings           []string             `json:"warnings"`
}

type cronNextParent struct {
	UID       string                    `json:"uid"`
	ProjectID int64                     `json:"project_id"`
	Status    string                    `json:"status"`
	Revision  int64                     `json:"revision"`
	DeletedAt *time.Time                `json:"deleted_at,omitempty"`
	Metadata  map[string]jsontext.Value `json:"metadata"`
}

func newCronNextCmd() *cobra.Command {
	options := cronNextOptions{count: 3}
	cmd := &cobra.Command{
		Use:   "next <uid>",
		Short: "preview future occurrences of a saved job without changing it",
		Long: `Preview future schedule instants from the saved definition and native progress.
Saved interval timing and delivery history stay unchanged. Paused
jobs and closed parents have no next occurrences. Adapter cron and once times
are trigger times; they do not promise execution. Other adapter triggers are
unavailable. State may change between reads. This preview neither reserves work nor replays missed occurrences.
--from never rewinds progress.`,
		Example: "  kata cron job next <job-uid> --count 3 --json\n  kata cron job next <job-uid> --from 2026-11-08T12:00:00Z",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return options.execute(cmd, args[0])
		},
	}
	cmd.Flags().IntVar(&options.count, "count", 3, "number of future occurrences (1–100)")
	cmd.Flags().StringVar(&options.from, "from", "", "RFC3339 lower bound (defaults to now)")
	return cmd
}

func (o cronNextOptions) execute(cmd *cobra.Command, id string) error {
	id, err := normalizedCronCLIUID(id)
	if err != nil {
		return err
	}
	if o.count < 1 || o.count > 100 {
		return cronCLIValidation("count must be between 1 and 100")
	}
	from := time.Now().UTC()
	if cmd.Flags().Changed("from") {
		from, err = time.Parse(time.RFC3339, o.from)
		if err != nil {
			return cronCLIValidation("from must be RFC3339")
		}
		from = from.UTC()
	}
	if _, err := from.MarshalText(); err != nil {
		return cronCLIValidation("from must represent an RFC3339 UTC instant")
	}
	daemon, err := dialDaemon(cmd.Context())
	if err != nil {
		return err
	}
	var project projectRef
	if strings.TrimSpace(flags.Project) != "" {
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
	result, err := readCronNext(daemon, client, project.ID, id, from, o.count)
	if err != nil {
		return err
	}
	return emitCronNext(cmd, result)
}

func readCronNext(a daemonAPI, c *kataclient.Client, projectID int64, id string, from time.Time, count int) (cronNextResult, error) {
	raw, err := (cronCLIOptions{resource: "job", action: "show", identity: id}).callRead(a, c, projectID)
	if err != nil {
		return cronNextResult{}, err
	}
	var envelope struct {
		Job struct {
			UID                string         `json:"uid"`
			DefinitionEventUID string         `json:"definition_event_uid"`
			DeletedAt          *time.Time     `json:"deleted_at"`
			Definition         jsontext.Value `json:"definition"`
		} `json:"job"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return cronNextResult{}, fmt.Errorf("decode cron job: %w", err)
	}
	job := envelope.Job
	def, err := cron.DecodeJob(job.Definition)
	if err != nil {
		return cronNextResult{}, fmt.Errorf("cannot preview saved job with this kata: %w", err)
	}
	zone := cron.TriggerLocation(def.Trigger)
	result := cronNextResult{JobUID: job.UID, DefinitionEventUID: job.DefinitionEventUID, AsOf: from.UTC(), Delivery: "adapter", Basis: "adapter_trigger", Timezone: zone.String(), Status: "ready", Occurrences: []cronNextOccurrence{}, Warnings: []string{}}
	native := cron.IsRecurringIssueJob(def)
	if native {
		result.Delivery, result.Basis = "daemon", "stored_progress"
	} else {
		result.Warnings = append(result.Warnings, "adapter trigger times do not promise execution")
	}
	if job.DeletedAt != nil {
		result.Status = "deleted"
		return result, nil
	}
	var parent *cron.PreviewParent
	if def.Enabled && native {
		wire, callErr := c.ShowIssueWithResponse(a.ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: projectID, Ref: def.Issue.ParentUID}, Query: &generated.ShowIssueQuery{IncludeDeleted: new(true)}})
		if wire == nil {
			return result, externalCLITransportError(wire, callErr)
		}
		if err := externalCLIResponseError(wire.StatusCode, wire.Body, callErr); err != nil {
			var cli *cliError
			if !errors.As(err, &cli) || cli.Code != "issue_not_found" {
				return result, err
			}
		} else {
			var response struct {
				Issue *cronNextParent `json:"issue"`
			}
			if err := json.Unmarshal(wire.Body, &response); err != nil {
				return result, fmt.Errorf("decode cron parent: %w", err)
			}
			if p := response.Issue; p != nil && p.UID == def.Issue.ParentUID && p.ProjectID == projectID && (p.Status == "open" || p.Status == "closed") {
				result.ParentRevision = p.Revision
				parent = &cron.PreviewParent{Open: p.Status == "open" && p.DeletedAt == nil}
				if progress, err := cron.DecodeRecurringProgress(p.Metadata["cron.progress."+job.UID]); err == nil {
					parent.Progress = &progress
				}
			}
		}
	}
	preview, err := cron.PreviewJob(def, parent, from, count)
	if err != nil {
		return result, fmt.Errorf("cannot preview saved job with this kata: %w", err)
	}
	result.Status = preview.Status
	if preview.Status == "unavailable" || preview.Status == "unsupported" {
		if preview.Status == "unsupported" {
			result.Basis = "adapter_defined"
		}
		return cronNextUnavailable(result, "saved schedule or parent is unavailable")
	}
	for _, at := range preview.Times {
		local, err := at.In(zone).MarshalText()
		if err != nil {
			return result, err
		}
		result.Occurrences = append(result.Occurrences, cronNextOccurrence{At: at.UTC(), Local: string(local)})
	}
	return result, nil
}

func cronNextUnavailable(result cronNextResult, reason string) (cronNextResult, error) {
	result.Status = "unavailable"
	result.Warnings = append(result.Warnings, reason)
	raw, err := json.Marshal(result)
	if err != nil {
		return cronNextResult{}, err
	}
	return result, &cliError{Kind: kindValidation, Code: "cron_preview_unavailable", ExitCode: ExitValidation,
		Message: "cron preview unavailable: " + reason, Data: raw}
}

func emitCronNext(cmd *cobra.Command, result cronNextResult) error {
	if currentOutputMode() == outputJSON || currentOutputMode() == outputAgent {
		return emitJSON(cmd.OutOrStdout(), result)
	}
	if flags.Quiet {
		return nil
	}
	label := "Next native occurrences"
	if result.Delivery == "adapter" {
		label = "Adapter trigger times"
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s (%s, %s):\n", label, result.Timezone, result.Status); err != nil {
		return err
	}
	if len(result.Occurrences) == 0 {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "  "+result.Status); err != nil {
			return err
		}
	}
	for _, occurrence := range result.Occurrences {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "  "+occurrence.Local); err != nil {
			return err
		}
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), warning); err != nil {
			return err
		}
	}
	return nil
}
