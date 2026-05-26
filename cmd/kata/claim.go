package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

func newClaimCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "claim <issue-ref>",
		Short: "claim ownership of an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClaim(cmd, args[0], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "force claim even if already owned by another actor")
	addCommentFlag(cmd)
	return cmd
}

func runClaim(cmd *cobra.Command, raw string, force bool) error {
	comment, err := commentFromFlag(cmd)
	if err != nil {
		return err
	}
	ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, raw)
	if err != nil {
		return err
	}
	actor, _ := resolveActor(flags.As, nil)
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	body := map[string]any{"actor": actor, "force": force}
	postURL := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/claim", baseURL, pid, url.PathEscape(issue.RefForAPI))
	status, bs, err := httpDoJSON(ctx, client, http.MethodPost, postURL, body)
	if err != nil {
		return err
	}
	if status >= 400 {
		return apiErrFromBody(status, bs)
	}
	if err := postFollowupComment(ctx, client, baseURL, pid, issue.RefForAPI, actor, comment); err != nil {
		return err
	}
	return printClaimMutation(cmd, bs)
}

// printClaimMutation formats the claim response for the selected output mode.
// Quiet human mode prints nothing; JSON mode emits the daemon body under the
// kata_api_version envelope; agent mode uses the mutation contract.
func printClaimMutation(cmd *cobra.Command, bs []byte) error {
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if mode == outputAgent {
		return printAgentMutation(cmd, "claim", bs, func(w io.Writer, m agentIssueMutation) error {
			if m.Issue.Owner != nil {
				if err := writeAgentField(w, "Owner", agentValue(*m.Issue.Owner)); err != nil {
					return err
				}
			}
			if m.PreviousOwner != nil && *m.PreviousOwner != "" {
				return writeAgentField(w, "Previous-Owner", agentValue(*m.PreviousOwner))
			}
			return nil
		})
	}
	var b struct {
		Issue struct {
			ShortID string  `json:"short_id"`
			Owner   *string `json:"owner"`
		} `json:"issue"`
		Changed       bool    `json:"changed"`
		PreviousOwner *string `json:"previous_owner,omitempty"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	if flags.Quiet {
		return nil
	}
	if !b.Changed {
		owner := ""
		if b.Issue.Owner != nil {
			owner = *b.Issue.Owner
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s already claimed by %s (no-op)\n", b.Issue.ShortID, owner)
		return err
	}
	owner := ""
	if b.Issue.Owner != nil {
		owner = *b.Issue.Owner
	}
	if b.PreviousOwner != nil && *b.PreviousOwner != "" {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s claimed by %s (was: %s)\n", b.Issue.ShortID, owner, *b.PreviousOwner)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s claimed by %s\n", b.Issue.ShortID, owner)
	return err
}
