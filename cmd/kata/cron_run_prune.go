package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func parseCronPruneAge(value string) (time.Duration, error) {
	age, err := parseDurationWithDays(value)
	// Prune accepts whole days, while digest also accepts fractional days.
	if days, ok := strings.CutSuffix(value, "d"); ok && err == nil {
		_, err = strconv.ParseUint(days, 10, 64)
	}
	if err != nil || age < 24*time.Hour {
		return 0, cronCLIValidation("--older-than must be at least 24h; use a duration such as 720h or whole days such as 30d")
	}
	return age, nil
}

func newCronRunPruneCmd() *cobra.Command {
	var olderThan, job, workflow, confirm string
	var keep, limit int64
	var force bool
	cmd := &cobra.Command{
		Use: "prune", Short: "preview or remove old terminal run evidence on the owning hub",
		Long: `Preview one batch of old terminal runs.
Each job or workflow keeps its newest --keep runs across all statuses;
running runs and recently updated runs are always retained. Export history first.
Each nonempty deletion resets browser streams and federation replicas.
Counts describe this batch. Preview each batch before deleting it.`,
		Example: "  kata cron run prune --older-than 30d --keep 10\n  kata cron run prune --older-than 30d --force --confirm 'PRUNE example-project cron runs' --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			age, err := parseCronPruneAge(olderThan)
			if err != nil {
				return err
			}
			if keep < 1 || keep > 1000 || limit < 1 || limit > 20000 {
				return cronCLIValidation("--keep must be 1..1000 and --limit must be 1..20000")
			}
			for name, value := range map[string]*string{"job": &job, "workflow": &workflow} {
				if cmd.Flags().Changed(name) || *value != "" {
					*value, err = normalizedCronCLIUID(*value)
					if err != nil {
						return err
					}
				}
			}
			a, err := dialDaemon(cmd.Context())
			if err != nil {
				return err
			}
			var project projectRef
			if strings.TrimSpace(flags.Project) != "" {
				project, err = resolveProjectSelectorIncludingArchived(a, flags.Project)
			} else {
				project, err = resolveProjectArgFlagOrWorkspace(a, nil)
			}
			if err != nil {
				return err
			}
			if force {
				expected := "PRUNE " + project.Name + " cron runs"
				confirm, err = resolveConfirm(cmd, confirm, expected, "Type "+expected+" to remove run evidence: ")
				if err != nil {
					return err
				}
			}
			actor, _ := resolveActor(cmd.Context(), flags.As, nil)
			// Pruning and post-commit baseline refresh each have a 30s budget.
			httpClient, err := longRunningClientForResolved(cmd.Context(), a.resolved)
			if err != nil {
				return err
			}
			client, err := kataclient.NewWithHTTPClient(a.baseURL, httpClient)
			if err != nil {
				return err
			}
			response, err := client.PruneCronRunsWithResponse(cmd.Context(), &generated.PruneCronRunsRequestOptions{
				PathParams: &generated.PruneCronRunsPath{ProjectID: project.ID},
				Header:     &generated.PruneCronRunsHeaders{XKataConfirm: &confirm},
				Body:       &generated.PruneCronRunsBody{Actor: &actor, Before: time.Now().Add(-age).UTC(), Keep: &keep, Limit: &limit, JobUID: optionalString(job), WorkflowUID: optionalString(workflow), DryRun: !force},
			})
			if response == nil {
				return externalCLITransportError(response, err)
			}
			raw, err := cronCLIResponse(response.StatusCode, response.Body, err)
			if err != nil {
				return err
			}
			var result db.CronRunPruneResult
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			if currentOutputMode() == outputJSON || currentOutputMode() == outputAgent {
				return emitJSON(cmd.OutOrStdout(), result)
			}
			if flags.Quiet {
				return nil
			}
			return renderCronRunPrune(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "minimum age, e.g. 30d or 720h (at least 24h)")
	_ = cmd.MarkFlagRequired("older-than")
	cmd.Flags().Int64Var(&keep, "keep", 10, "newest runs to retain per job or workflow (1..1000)")
	cmd.Flags().Int64Var(&limit, "limit", 5000, "maximum runs in this batch (1..20000)")
	cmd.Flags().StringVar(&job, "job", "", "select one job UID")
	cmd.Flags().StringVar(&workflow, "workflow", "", "select one workflow UID")
	cmd.MarkFlagsMutuallyExclusive("job", "workflow")
	cmd.Flags().BoolVar(&force, "force", false, "delete the selected batch instead of previewing")
	cmd.Flags().StringVar(&confirm, "confirm", "", "exact confirmation: PRUNE <project> cron runs")
	return cmd
}

func renderCronRunPrune(out io.Writer, result db.CronRunPruneResult) error {
	verb := "Pruned"
	if result.DryRun {
		verb = "Would prune"
	}
	if _, err := fmt.Fprintf(out, "%s %d runs and %d observation events (more: %t).\n", verb, result.Pruned, result.EventsDeleted, result.More); err != nil {
		return err
	}
	if result.ResetAfterEventID > 0 {
		if _, err := fmt.Fprintln(out, "Replicas will rebuild."); err != nil {
			return err
		}
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "KIND\tUID\tNAME\tSUCCEEDED\tFAILED\tCANCELLED\tUNKNOWN\tOLDEST\tNEWEST"); err != nil {
		return err
	}
	for _, group := range result.Groups {
		kind, id := "workflow", group.WorkflowUID
		if group.JobUID != nil {
			kind, id = "job", group.JobUID
		}
		if id == nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n", kind, textsafe.Line(*id), textsafe.Line(group.Name), group.Counts["succeeded"], group.Counts["failed"], group.Counts["cancelled"], group.Counts["unknown"], group.OldestCreatedAt.UTC().Format(time.RFC3339Nano), group.NewestCreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return w.Flush()
}
