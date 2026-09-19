package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"time"

	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"

	"github.com/spf13/cobra"
)

func newClaimCmd() *cobra.Command {
	var force, ifUnowned bool
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "claim <issue-ref>",
		Short: "claim ownership of an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ttlSeconds, err := assignmentTTLSeconds(cmd, ttl)
			if err != nil {
				return err
			}
			return runClaim(cmd, args[0], force, ifUnowned, ttlSeconds)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "force claim even if already owned by another actor")
	cmd.Flags().BoolVar(&ifUnowned, "if-unowned", false, "claim only if the issue has no owner")
	cmd.Flags().Var(durationFlag{d: &ttl}, "ttl", "expire the assignment after a duration from 1m through 24h")
	cmd.MarkFlagsMutuallyExclusive("force", "if-unowned")
	addCommentFlag(cmd)
	return cmd
}

func assignmentTTLSeconds(cmd *cobra.Command, ttl time.Duration) (*int64, error) {
	if !cmd.Flags().Changed("ttl") {
		return nil, nil
	}
	if ttl%time.Second != 0 {
		return nil, fmt.Errorf("ttl must be a whole number of seconds")
	}
	if ttl < time.Minute || ttl > 24*time.Hour {
		return nil, fmt.Errorf("ttl must be between 1m and 24h")
	}
	seconds := int64(ttl / time.Second)
	return &seconds, nil
}

func runClaim(cmd *cobra.Command, raw string, force, ifUnowned bool, ttlSeconds *int64) error {
	comment, handle, err := prepareFollowupComment(cmd)
	if err != nil {
		return err
	}
	ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, raw)
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
	response, callErr := apiClient.ClaimIssueWithResponse(ctx, &generated.ClaimIssueRequestOptions{PathParams: &generated.ClaimIssuePath{ProjectID: pid, Ref: issue.RefForAPI}, Body: &generated.ClaimIssueBody{Actor: &actor, Force: &force, IfUnowned: &ifUnowned, TTLSeconds: ttlSeconds}})
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return err
	}
	bs := response.Body
	if err := postFollowupComment(ctx, client, baseURL, pid, issue.RefForAPI, actor, comment, handle); err != nil {
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
		if err := emitJSON(&buf, jsontext.Value(bs)); err != nil {
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
			if m.Issue.AssignmentExpiresOn != nil {
				if err := writeAgentField(w, "Assignment-Expires-On", agentValue(*m.Issue.AssignmentExpiresOn)); err != nil {
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
			ShortID             string  `json:"short_id"`
			Owner               *string `json:"owner"`
			AssignmentExpiresOn *string `json:"assignment_expires_on"`
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
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s already assigned to %s%s (no-op)\n",
			b.Issue.ShortID, owner, assignmentExpirySuffix(b.Issue.AssignmentExpiresOn))
		return err
	}
	owner := ""
	if b.Issue.Owner != nil {
		owner = *b.Issue.Owner
	}
	if b.PreviousOwner != nil && *b.PreviousOwner != "" {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s assigned to %s%s (was: %s)\n",
			b.Issue.ShortID, owner, assignmentExpirySuffix(b.Issue.AssignmentExpiresOn), *b.PreviousOwner)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s assigned to %s%s\n",
		b.Issue.ShortID, owner, assignmentExpirySuffix(b.Issue.AssignmentExpiresOn))
	return err
}

func assignmentExpirySuffix(expiresOn *string) string {
	if expiresOn == nil {
		return ""
	}
	return " until " + *expiresOn
}
