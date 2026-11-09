package main

import (
	"context"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newCronRequestWriteCmd(cancel bool) *cobra.Command {
	action, short := "request-run", "ask an optional runtime to run an execute job now"
	if cancel {
		action, short = "request-cancel", "ask an optional runtime to cancel a running run"
	}
	var requestUID string
	var ttl int64
	cmd := &cobra.Command{Use: action + " <uid>", Short: short, Args: cobra.ExactArgs(1), Long: "Submit an advisory request at the project's owning hub. Supply a fresh ULID, retain it with the TTL, and reuse both after a lost response. Request admission launches or cancels nothing itself."}
	cmd.Flags().StringVar(&requestUID, "request-uid", "", "request ULID; retain for retry")
	cmd.Flags().Int64Var(&ttl, "ttl", 600, "request lifetime in seconds (60..86400)")
	_ = cmd.MarkFlagRequired("request-uid")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runCronProjectCommand(cmd, true, func(ctx context.Context, c *kataclient.Client, projectID int64, actor string) ([]byte, error) {
			target := strings.ToUpper(strings.TrimSpace(args[0]))
			body := &generated.CronRequestBody{UID: strings.ToUpper(strings.TrimSpace(requestUID)), Actor: &actor, TTLSeconds: &ttl}
			if cancel {
				wire, err := c.RequestCronRunCancelWithResponse(ctx, &generated.RequestCronRunCancelRequestOptions{PathParams: &generated.RequestCronRunCancelPath{ProjectID: projectID, RunUID: target}, Body: body})
				if wire == nil {
					return nil, externalCLITransportError(wire, err)
				}
				return cronCLIResponse(wire.StatusCode, wire.Body, err)
			}
			wire, err := c.RequestCronJobRunWithResponse(ctx, &generated.RequestCronJobRunRequestOptions{PathParams: &generated.RequestCronJobRunPath{ProjectID: projectID, CronUID: target}, Body: body})
			if wire == nil {
				return nil, externalCLITransportError(wire, err)
			}
			return cronCLIResponse(wire.StatusCode, wire.Body, err)
		})
	}
	return cmd
}

func newCronRequestListCmd() *cobra.Command {
	var state string
	cmd := &cobra.Command{Use: "list", Short: "list cron run and cancel requests with their answer state", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&state, "state", "", "filter pending, answered or expired requests")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return runCronProjectCommand(cmd, false, func(ctx context.Context, c *kataclient.Client, projectID int64, _ string) ([]byte, error) {
			query := &generated.ListCronRequestsQuery{}
			if state != "" {
				query.State = new(generated.ListCronRequestsQueryState(state))
			}
			wire, err := c.ListCronRequestsWithResponse(ctx, &generated.ListCronRequestsRequestOptions{PathParams: &generated.ListCronRequestsPath{ProjectID: projectID}, Query: query})
			if wire == nil {
				return nil, externalCLITransportError(wire, err)
			}
			return cronCLIResponse(wire.StatusCode, wire.Body, err)
		})
	}
	return cmd
}
