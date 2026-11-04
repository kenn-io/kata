package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newCronRunCountsCmd() *cobra.Command {
	var job, workflow string
	command := &cobra.Command{Use: "counts", Short: "show retained runs and recorded pruning counts", Long: `Read current retained-run counts and pruned-since-tracking counts from one snapshot.
Pruned counts start with the first contributing prune. Replayed evidence that is
pruned again contributes again. Filters select a job or workflow's own group;
--workflow does not include job runs belonging to that workflow.`, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		for name, value := range map[string]*string{"job": &job, "workflow": &workflow} {
			if cmd.Flags().Changed(name) || *value != "" {
				normalized, err := normalizedCronCLIUID(*value)
				if err != nil {
					return err
				}
				*value = normalized
			}
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
		response, err := client.GetCronRunCountsWithResponse(cmd.Context(), &generated.GetCronRunCountsRequestOptions{PathParams: &generated.GetCronRunCountsPath{ProjectID: project.ID}, Query: &generated.GetCronRunCountsQuery{JobUID: &job, WorkflowUID: &workflow}})
		if response == nil {
			return externalCLITransportError(response, err)
		}
		raw, err := cronCLIResponse(response.StatusCode, response.Body, err)
		if err != nil {
			return err
		}
		var result db.CronRunCountsResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return err
		}
		if currentOutputMode() == outputJSON || currentOutputMode() == outputAgent {
			return emitJSON(cmd.OutOrStdout(), result)
		}
		if flags.Quiet {
			return nil
		}
		return renderCronRunCounts(cmd.OutOrStdout(), result)
	}}
	command.Flags().StringVar(&job, "job", "", "select one job UID")
	command.Flags().StringVar(&workflow, "workflow", "", "select one workflow UID")
	command.MarkFlagsMutuallyExclusive("job", "workflow")
	return command
}

func renderCronRunCounts(out io.Writer, result db.CronRunCountsResult) error {
	if len(result.Groups) == 0 {
		_, err := fmt.Fprintln(out, "No retained runs or recorded pruning.")
		return err
	}
	for _, group := range result.Groups {
		if _, err := fmt.Fprintf(out, "%s %s  %s\n", textsafe.Line(group.Kind), textsafe.Line(group.UID), textsafe.Line(group.Name)); err != nil {
			return err
		}
		r, p := group.Retained, group.PrunedSinceTracking
		if _, err := fmt.Fprintf(out, "  Retained: running=%d succeeded=%d failed=%d cancelled=%d unknown=%d\n  Pruned since tracking: succeeded=%d failed=%d cancelled=%d unknown=%d\n", r.Running, r.Succeeded, r.Failed, r.Cancelled, r.Unknown, p.Succeeded, p.Failed, p.Cancelled, p.Unknown); err != nil {
			return err
		}
		for _, field := range []struct {
			label string
			at    *time.Time
		}{{"Coverage started", group.CoverageStartedAt}, {"Last pruned", group.LastPrunedAt}, {"Oldest pruned run", group.OldestCreatedAt}, {"Newest pruned run", group.NewestCreatedAt}} {
			if field.at != nil {
				if _, err := fmt.Fprintf(out, "  %s: %s\n", field.label, field.at.UTC().Format(time.RFC3339Nano)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
