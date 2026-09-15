package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newAssignCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assign <issue-ref> <owner>",
		Short: "set the owner of an issue",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAssign(cmd, args[0], args[1], false, nil)
		},
	}
	addCommentFlag(cmd)
	return cmd
}

func newUnassignCmd() *cobra.Command {
	var expectedOwner string
	cmd := &cobra.Command{
		Use:   "unassign <issue-ref>",
		Short: "clear the owner of an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var expected *string
			if cmd.Flags().Changed("expect-owner") {
				trimmed := strings.TrimSpace(expectedOwner)
				if trimmed == "" {
					return &cliError{
						Message: "--expect-owner must not be empty", Kind: kindValidation, ExitCode: ExitValidation,
					}
				}
				expected = &trimmed
			}
			return runAssign(cmd, args[0], "", true, expected)
		},
	}
	cmd.Flags().StringVar(&expectedOwner, "expect-owner", "", "clear ownership only if it matches this owner")
	addCommentFlag(cmd)
	return cmd
}

func runAssign(cmd *cobra.Command, raw, owner string, unassign bool, expectedOwner *string) error {
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
	var bs []byte
	if unassign {
		response, callErr := apiClient.UnassignIssueWithResponse(ctx, &generated.UnassignIssueRequestOptions{
			PathParams: &generated.UnassignIssuePath{ProjectID: pid, Ref: issue.RefForAPI},
			Body:       &generated.UnassignIssueBody{Actor: &actor, ExpectedOwner: expectedOwner},
		})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		bs = response.Body
	} else {
		response, callErr := apiClient.AssignIssueWithResponse(ctx, &generated.AssignIssueRequestOptions{
			PathParams: &generated.AssignIssuePath{ProjectID: pid, Ref: issue.RefForAPI},
			Body:       &generated.AssignIssueBody{Actor: &actor, Owner: owner},
		})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		bs = response.Body
	}
	if err := postFollowupComment(ctx, client, baseURL, pid, issue.RefForAPI, actor, comment, handle); err != nil {
		return err
	}
	return printAssignMutation(cmd, bs, unassign)
}

// printAssignMutation formats the assign/unassign response. Quiet mode prints
// nothing; JSON mode emits the daemon body under the kata_api_version
// envelope; otherwise prints a single human-readable line.
func printAssignMutation(cmd *cobra.Command, bs []byte, unassign bool) error {
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
		verb := "assign"
		if unassign {
			verb = "unassign"
		}
		var m agentIssueMutation
		if err := json.Unmarshal(bs, &m); err != nil {
			return err
		}
		return printAgentMutationDecoded(cmd.OutOrStdout(), verb, m, true, func(w io.Writer, m agentIssueMutation) error {
			if unassign {
				return writeAgentField(w, "Owner-Cleared", "true")
			}
			if m.Issue.Owner != nil {
				return writeAgentField(w, "Owner", agentValue(*m.Issue.Owner))
			}
			return nil
		})
	}
	var b struct {
		Issue struct {
			ShortID string  `json:"short_id"`
			Owner   *string `json:"owner"`
		} `json:"issue"`
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	if flags.Quiet {
		return nil
	}
	if !b.Changed {
		state := "unassigned"
		if b.Issue.Owner != nil {
			state = "assigned to " + *b.Issue.Owner
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s already %s (no-op)\n", b.Issue.ShortID, state)
		return err
	}
	if unassign {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s unassigned\n", b.Issue.ShortID)
		return err
	}
	owner := ""
	if b.Issue.Owner != nil {
		owner = *b.Issue.Owner
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s assigned to %s\n", b.Issue.ShortID, owner)
	return err
}
