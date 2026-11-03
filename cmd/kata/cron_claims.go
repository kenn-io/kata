package main

import (
	"fmt"
	"strings"

	"encoding/json/jsontext"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newCronClaimCmd() *cobra.Command {
	var job, event, key, workflow, workflowEvent, executor string
	command := &cobra.Command{Use: "claim <run-uid>", Short: "reserve an occurrence at its root before launching", Args: cobra.ExactArgs(1), Long: `Reserve an occurrence using a persisted, unique attempt UID. Reuse the same UID
and flags after a timeout. A replay confirms a reservation; consult the adapter's
durable execution state before recovering it. Conflict and pending return errors;
never launch without a grant. Reservations have no expiry or takeover.`}
	execute := func(cmd *cobra.Command, id string, show bool) error {
		run, err := normalizedCronCLIUID(id)
		if err != nil {
			return err
		}
		var body generated.CronClaimBody
		if !show {
			for _, value := range []*string{&job, &event} {
				*value, err = normalizedCronCLIUID(*value)
				if err != nil {
					return err
				}
			}
			if strings.TrimSpace(key) == "" {
				return cronCLIValidation("--occurrence-key is required")
			}
			if (workflow == "") != (workflowEvent == "") {
				return cronCLIValidation("workflow UID and definition event UID must be paired")
			}
			for _, value := range []*string{&workflow, &workflowEvent} {
				if *value != "" {
					*value, err = normalizedCronCLIUID(*value)
					if err != nil {
						return err
					}
				}
			}
			actor, _ := resolveActor(cmd.Context(), flags.As, nil)
			body = generated.CronClaimBody{RunUID: run, DefinitionEventUID: event, OccurrenceKey: key, Actor: &actor}
			if executor != "" {
				body.ExecutorLabel = &executor
			}
			selected, err := resolveTeammate(cmd)
			if err != nil {
				return err
			}
			if selected != "" {
				body.Teammate = &selected
			}
			if workflow != "" {
				body.WorkflowUID = &workflow
				body.WorkflowDefinitionEventUID = &workflowEvent
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
		var raw []byte
		if show {
			response, callErr := client.ShowCronOccurrenceClaimWithResponse(cmd.Context(), &generated.ShowCronOccurrenceClaimRequestOptions{PathParams: &generated.ShowCronOccurrenceClaimPath{ProjectID: project.ID, RunUID: run}})
			if response == nil {
				return externalCLITransportError(response, callErr)
			}
			raw, err = cronCLIResponse(response.StatusCode, response.Body, callErr)
		} else {
			response, callErr := client.ClaimCronOccurrenceWithResponse(cmd.Context(), &generated.ClaimCronOccurrenceRequestOptions{PathParams: &generated.ClaimCronOccurrencePath{ProjectID: project.ID, JobUID: job}, Body: &body})
			if response == nil {
				return externalCLITransportError(response, callErr)
			}
			raw, err = cronCLIResponse(response.StatusCode, response.Body, callErr)
		}
		if err != nil {
			return err
		}
		return emitJSON(cmd.OutOrStdout(), jsontext.Value(raw))
	}
	command.RunE = func(cmd *cobra.Command, args []string) error { return execute(cmd, args[0], false) }
	command.Flags().StringVar(&job, "job-uid", "", "job ULID (required)")
	command.Flags().StringVar(&event, "definition-event-uid", "", "job definition event ULID (required)")
	command.Flags().StringVar(&key, "occurrence-key", "", "canonical occurrence key (required)")
	command.Flags().StringVar(&executor, "executor-label", "", "stable executor label")
	command.Flags().StringVar(&workflow, "workflow-uid", "", "optional workflow ULID")
	command.Flags().StringVar(&workflowEvent, "workflow-definition-event-uid", "", "paired workflow definition event ULID")
	show := &cobra.Command{Use: "show <run-uid>", Short: "look up a retained grant after an uncertain response", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return execute(cmd, args[0], true) }}
	command.AddCommand(show)
	command.Example = fmt.Sprintf("  kata cron run claim <run-uid> --job-uid <job-uid> --definition-event-uid <event-uid> --occurrence-key %s --json\n  kata cron run claim show <run-uid> --json", "2030-01-02T03:04:05Z")
	return command
}
