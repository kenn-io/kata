package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newLabelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "label",
		Short: "add or remove a label on an issue",
	}
	cmd.AddCommand(labelAddCmd(), labelRmCmd())
	return cmd
}

func labelAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <issue-ref> <label>",
		Short: "attach a label to an issue",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			label := args[1]
			if strings.TrimSpace(label) == "" {
				return &cliError{Message: "label must not be empty", Kind: kindValidation, ExitCode: ExitValidation}
			}
			comment, handle, err := prepareFollowupComment(cmd)
			if err != nil {
				return err
			}
			project, issue, err := prepareIssueMutation(cmd, args[0], false)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(project.api.ctx, flags.As, nil)
			apiClient, err := project.generatedClient()
			if err != nil {
				return err
			}
			response, callErr := apiClient.AddLabelWithResponse(project.api.ctx, &generated.AddLabelRequestOptions{
				PathParams: &generated.AddLabelPath{ProjectID: project.selector, Ref: issue.RefForAPI},
				Body:       &generated.AddLabelBody{Actor: &actor, Label: label},
			})
			if err := externalCLITransportError(response, callErr); err != nil {
				return err
			}
			bs, err := project.finishMutation(response.HTTPResponse, response.Body, callErr)
			if err != nil {
				return err
			}
			if err := project.comment(bs, issue.RefForAPI, actor, comment, handle); err != nil {
				return err
			}
			return printLabelMutation(cmd, bs)
		},
	}
	addCommentFlag(cmd)
	return cmd
}

func labelRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <issue-ref> <label>",
		Short: "detach a label from an issue",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			label := args[1]
			// Empty label here used to URL-encode to "" and hit
			// /labels/?actor=... which the daemon answered with a
			// raw 404 page. Reject client-side so the user gets a
			// meaningful message — hammer-test finding #8.
			if strings.TrimSpace(label) == "" {
				return &cliError{Message: "label must not be empty", Kind: kindValidation, ExitCode: ExitValidation}
			}
			comment, handle, err := prepareFollowupComment(cmd)
			if err != nil {
				return err
			}
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
			response, callErr := apiClient.RemoveLabelWithResponse(ctx, &generated.RemoveLabelRequestOptions{
				PathParams: &generated.RemoveLabelPath{ProjectID: pid, Ref: issue.RefForAPI, Label: label},
				Query:      &generated.RemoveLabelQuery{Actor: &actor},
			})
			if err := externalCLITransportError(response, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
				return err
			}
			bs := response.Body
			if err := postFollowupComment(ctx, client, baseURL, pid, issue.RefForAPI, actor, comment, handle); err != nil {
				return err
			}
			return printLabelRemoved(cmd, bs, issue.RefForAPI, label)
		},
	}
	addCommentFlag(cmd)
	return cmd
}

func newLabelsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "labels",
		Short: "list label counts in this project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			start, err := resolveStartPath(flags.Workspace)
			if err != nil {
				return err
			}
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			pid, err := resolveProjectID(ctx, baseURL, start)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
			if err != nil {
				return err
			}
			response, callErr := apiClient.ListLabelsWithResponse(ctx, &generated.ListLabelsRequestOptions{
				PathParams: &generated.ListLabelsPath{ProjectID: pid},
			})
			if err := externalCLITransportError(response, callErr); err != nil {
				return err
			}
			if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
				return err
			}
			bs := response.Body
			mode := currentOutputMode()
			if mode == outputJSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, jsontext.Value(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			var b struct {
				Labels []struct {
					Label string `json:"label"`
					Count int64  `json:"count"`
				} `json:"labels"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			if mode == outputAgent {
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "OK labels count=%d\n", len(b.Labels)); err != nil {
					return err
				}
				for _, c := range b.Labels {
					count := fmt.Sprint(c.Count)
					if err := writeAgentKVRow(out,
						agentRowField("label", c.Label),
						agentRowField("count", count),
					); err != nil {
						return err
					}
				}
				return nil
			}
			for _, c := range b.Labels {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-32s  %d\n", c.Label, c.Count); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// printLabelMutation formats AddLabelResponse for the three output modes.
func printLabelMutation(cmd *cobra.Command, bs []byte) error {
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
		var m agentIssueMutation
		if err := json.Unmarshal(bs, &m); err != nil {
			return err
		}
		return printAgentMutationDecoded(cmd.OutOrStdout(), "label", m, true, func(w io.Writer, m agentIssueMutation) error {
			if err := writeAgentField(w, "Label", agentValue(m.Label.Label)); err != nil {
				return err
			}
			return writeAgentField(w, "Action", "added")
		})
	}
	var b struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
		Label struct {
			Label string `json:"label"`
		} `json:"label"`
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	if flags.Quiet {
		return nil
	}
	if !b.Changed {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s already labeled %q (no-op)\n",
			b.Issue.ShortID, textsafe.Line(b.Label.Label))
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s labeled %q\n",
		b.Issue.ShortID, textsafe.Line(b.Label.Label))
	return err
}

// printLabelRemoved formats the DELETE-label response. The MutationResponse
// body carries only {issue, event, changed} so the line is built from the
// (issue ref, label) the CLI used to call DELETE.
func printLabelRemoved(cmd *cobra.Command, bs []byte, ref, label string) error {
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
		var m agentIssueMutation
		if err := json.Unmarshal(bs, &m); err != nil {
			return err
		}
		return printAgentMutationDecoded(cmd.OutOrStdout(), "label", m, true, func(w io.Writer, _ agentIssueMutation) error {
			if err := writeAgentField(w, "Label", agentValue(label)); err != nil {
				return err
			}
			return writeAgentField(w, "Action", "removed")
		})
	}
	var b struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	if flags.Quiet {
		return nil
	}
	if !b.Changed {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s label %q already removed (no-op)\n", ref, label)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s unlabeled %q\n", ref, label)
	return err
}
