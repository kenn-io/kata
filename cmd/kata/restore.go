package main

import (
	"encoding/json/v2"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// newRestoreCmd returns the cobra.Command for `kata restore`.
//
// Restore is the simplest of the destructive verbs (spec §3.5 step 4): no
// --force, no --confirm, no TTY prompt. POSTs to /actions/restore with just
// an actor; the daemon-side RestoreIssue is idempotent and returns
// changed=false when the issue isn't deleted.
func newRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <issue-ref>",
		Short: "restore a soft-deleted issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, args[0])
			if err != nil {
				return err
			}
			actor, _ := resolveActor(ctx, flags.As, nil)
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
			if err != nil {
				return err
			}
			response, callErr := apiClient.RestoreIssueWithResponse(ctx, &generated.RestoreIssueRequestOptions{
				PathParams: &generated.RestoreIssuePath{ProjectID: pid, Ref: issue.RefForAPI},
				Body:       &generated.RestoreIssueBody{Actor: actor},
			})
			if err := externalCLITransportError(response, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
				return err
			}
			bs := response.Body
			if currentOutputMode() == outputAgent {
				var m agentIssueMutation
				if err := json.Unmarshal(bs, &m); err != nil {
					return err
				}
				return printAgentMutationDecoded(cmd.OutOrStdout(), "restore", m, true, func(w io.Writer, _ agentIssueMutation) error {
					return writeAgentField(w, "Deleted", "false")
				})
			}
			if !flags.Quiet && currentOutputMode() == outputHuman {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s restored\n", issue.RefForAPI)
				return err
			}
			return printMutation(cmd, bs)
		},
	}
}
