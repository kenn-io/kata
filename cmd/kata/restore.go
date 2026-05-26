package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
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
			ctx, baseURL, pid, issue, err := resolveIssueRefForCommandWithOptions(cmd, args[0], true)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(flags.As, nil)
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
				fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/restore", baseURL, pid, url.PathEscape(issue.RefForAPI)),
				map[string]any{"actor": actor})
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
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
