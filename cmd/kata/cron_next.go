package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
	kataclient "go.kenn.io/kata/pkg/client"
)

type cronNextOccurrence struct {
	At    time.Time `json:"at"`
	Local string    `json:"local"`
}

type cronNextResult struct {
	JobUID             string               `json:"job_uid"`
	DefinitionEventUID string               `json:"definition_event_uid"`
	AsOf               time.Time            `json:"as_of"`
	Basis              string               `json:"basis"`
	Timezone           string               `json:"timezone"`
	Status             string               `json:"status"`
	Occurrences        []cronNextOccurrence `json:"occurrences"`
}

func readCronNext(a daemonAPI, c *kataclient.Client, projectID int64, id string, from time.Time, count int) (cronNextResult, error) {
	raw, err := (cronCLIOptions{}).callCapabilities(a, c, projectID)
	if err != nil {
		return cronNextResult{}, err
	}
	var capabilities struct {
		EventFeatures []string `json:"event_features"`
	}
	if err := json.Unmarshal(raw, &capabilities); err != nil {
		return cronNextResult{}, fmt.Errorf("decode cron capabilities: %w", err)
	}
	if !slices.Contains(capabilities.EventFeatures, db.CronEventFeature) {
		return cronNextResult{}, &cliError{Kind: kindValidation, Code: "cron_preview_unsupported", ExitCode: ExitValidation, Message: "saved-job preview is unsupported by this daemon"}
	}
	raw, err = (cronCLIOptions{resource: "job", action: "show", identity: id}).callRead(a, c, projectID)
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
	result := cronNextResult{JobUID: job.UID, DefinitionEventUID: job.DefinitionEventUID, AsOf: from.UTC(), Basis: "adapter_trigger", Timezone: zone.String(), Status: "ready", Occurrences: []cronNextOccurrence{}}
	native := cron.IsRecurringIssueJob(def)
	if native {
		result.Basis = "stored_progress"
	}
	if job.DeletedAt != nil {
		if err := def.ValidateVersion(); err != nil {
			return cronNextResult{}, fmt.Errorf("cannot preview saved job with this kata: %w", err)
		}
		result.Status = "deleted"
		return result, nil
	}
	var parent *cron.PreviewParent
	if def.Enabled && native {
		p, readErr := recurrenceclient.ReadCronParent(a.ctx, c.Client, projectID, def.Issue.ParentUID)
		if readErr != nil {
			mapped := recurrenceCLIError(readErr)
			var cli *cliError
			if !errors.As(mapped, &cli) || cli.Code != "issue_not_found" {
				return result, mapped
			}
		} else {
			parent = &cron.PreviewParent{Open: p.Status == "open" && p.DeletedAt == nil}
			if progress, err := cron.DecodeRecurringProgress(p.Metadata[recurrenceclient.CronProgressPrefix+job.UID]); err == nil {
				parent.Progress = &progress
			}
		}
	}
	preview, err := cron.PreviewJob(def, parent, from, count)
	if err != nil {
		return result, fmt.Errorf("cannot preview saved job with this kata: %w", err)
	}
	result.Status = preview.Status
	if preview.Status == "unavailable" || preview.Status == "unsupported" {
		return result, &cliError{Kind: kindValidation, Code: "cron_preview_unavailable", ExitCode: ExitValidation, Message: "cron preview unavailable"}
	}
	for _, at := range preview.Times {
		result.Occurrences = append(result.Occurrences, cronNextOccurrence{At: at.UTC(), Local: at.In(zone).Format(time.RFC3339Nano)})
	}
	return result, nil
}

func emitCronNext(cmd *cobra.Command, result cronNextResult) error {
	if currentOutputMode() == outputJSON || currentOutputMode() == outputAgent {
		return emitJSON(cmd.OutOrStdout(), result)
	}
	if flags.Quiet {
		return nil
	}
	label := "Next native occurrences"
	if result.Basis == "adapter_trigger" {
		label = "Adapter trigger times"
	}
	lines := []string{fmt.Sprintf("%s (%s, %s)", label, result.Timezone, strings.ReplaceAll(result.Status, "_", " "))}
	if len(result.Occurrences) > 0 {
		lines[0] += ":"
	}
	for _, occurrence := range result.Occurrences {
		lines = append(lines, "  "+occurrence.Local)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), strings.Join(lines, "\n"))
	return err
}
