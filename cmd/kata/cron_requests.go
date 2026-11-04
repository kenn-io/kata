package main

import (
	"encoding/json/jsontext"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func cronRequestClient(cmd *cobra.Command) (*kataclient.Client, projectRef, string, error) {
	daemon, err := dialDaemon(cmd.Context())
	if err != nil {
		return nil, projectRef{}, "", err
	}
	var project projectRef
	if strings.TrimSpace(flags.Project) != "" {
		project, err = resolveProjectSelectorIncludingArchived(daemon, flags.Project)
	} else {
		project, err = resolveProjectArgFlagOrWorkspace(daemon, nil)
	}
	if err != nil {
		return nil, projectRef{}, "", err
	}
	actor, _ := resolveActor(cmd.Context(), flags.As, nil)
	client, err := kataclient.NewWithHTTPClient(daemon.baseURL, daemon.client)
	return client, project, actor, err
}

func newCronRequestWriteCmd(cancel bool) *cobra.Command {
	action, kind := "request-run", "job"
	if cancel {
		action, kind = "request-cancel", "run"
	}
	var requestUID string
	var ttl int64
	cmd := &cobra.Command{Use: action + " <uid>", Short: "ask an optional runtime to " + map[bool]string{false: "run an execute job now", true: "cancel a running run"}[cancel], Args: cobra.ExactArgs(1), Long: "Submit an advisory request at the project's owning hub. Supply a fresh ULID, retain it with the TTL, and reuse both after a lost response. Request admission launches or cancels nothing itself."}
	cmd.Flags().StringVar(&requestUID, "request-uid", "", "request ULID; retain for retry")
	cmd.Flags().Int64Var(&ttl, "ttl", 600, "request lifetime in seconds (60..86400)")
	_ = cmd.MarkFlagRequired("request-uid")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		client, project, actor, err := cronRequestClient(cmd)
		if err != nil {
			return err
		}
		id := strings.ToUpper(strings.TrimSpace(requestUID))
		target := strings.ToUpper(strings.TrimSpace(args[0]))
		body := generated.CronRequestBody{UID: id, Actor: &actor, TTLSeconds: &ttl}
		var raw []byte
		if kind == "job" {
			wire, callErr := client.RequestCronJobRunWithResponse(cmd.Context(), &generated.RequestCronJobRunRequestOptions{PathParams: &generated.RequestCronJobRunPath{ProjectID: project.ID, CronUID: target}, Body: &body})
			if wire == nil {
				return externalCLITransportError(wire, callErr)
			}
			raw, err = cronCLIResponse(wire.StatusCode, wire.Body, callErr)
		} else {
			wire, callErr := client.RequestCronRunCancelWithResponse(cmd.Context(), &generated.RequestCronRunCancelRequestOptions{PathParams: &generated.RequestCronRunCancelPath{ProjectID: project.ID, RunUID: target}, Body: &body})
			if wire == nil {
				return externalCLITransportError(wire, callErr)
			}
			raw, err = cronCLIResponse(wire.StatusCode, wire.Body, callErr)
		}
		if err != nil {
			return err
		}
		if flags.Quiet && currentOutputMode() != outputJSON {
			return nil
		}
		return emitJSON(cmd.OutOrStdout(), jsontext.Value(raw))
	}
	return cmd
}

func newCronRequestListCmd() *cobra.Command {
	var state string
	cmd := &cobra.Command{Use: "list", Short: "list retained requests and their current answer state", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&state, "state", "", "filter pending, answered or expired requests")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		client, project, _, err := cronRequestClient(cmd)
		if err != nil {
			return err
		}
		query := &generated.ListCronRequestsQuery{}
		if state != "" {
			query.State = new(generated.ListCronRequestsQueryState(state))
		}
		wire, callErr := client.ListCronRequestsWithResponse(cmd.Context(), &generated.ListCronRequestsRequestOptions{PathParams: &generated.ListCronRequestsPath{ProjectID: project.ID}, Query: query})
		if wire == nil {
			return externalCLITransportError(wire, callErr)
		}
		raw, err := cronCLIResponse(wire.StatusCode, wire.Body, callErr)
		if err != nil {
			return err
		}
		return emitJSON(cmd.OutOrStdout(), jsontext.Value(raw))
	}
	return cmd
}
