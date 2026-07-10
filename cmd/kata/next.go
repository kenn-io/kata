package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newNextCmd() *cobra.Command {
	var (
		all      bool
		unowned  bool
		owner    string
		labels   []string
		noLabels []string
		full     bool
	)
	cmd := &cobra.Command{
		Use:   "next",
		Short: "show the highest-priority ready issue",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options := readyOptions{
				All: all, Unowned: unowned, Owner: owner,
				Labels: labels, NoLabels: noLabels,
			}
			result, err := options.fetch(cmd)
			if err != nil {
				return err
			}

			selected, found := selectNextReadyIssue(result.Issues)
			mode := currentOutputMode()
			if !found {
				switch mode {
				case outputJSON:
					return emitJSON(cmd.OutOrStdout(), struct {
						Issue json.RawMessage `json:"issue"`
					}{Issue: json.RawMessage("null")})
				case outputAgent:
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "OK next found=false")
					return err
				default:
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "No ready issues.")
					return err
				}
			}

			issueRef := selected.ShortID
			if all {
				issueRef = selected.ProjectName + "#" + selected.ShortID
			}
			if full {
				return runShow(cmd, issueRef, "next")
			}
			switch mode {
			case outputJSON:
				return emitJSON(cmd.OutOrStdout(), struct {
					Issue json.RawMessage `json:"issue"`
				}{Issue: selected.Raw})
			case outputAgent:
				return writeNextAgent(cmd.OutOrStdout(), issueRef, selected)
			default:
				owner := "-"
				if selected.Owner != nil {
					owner = *selected.Owner
				}
				return newRowRenderer(cmd.OutOrStdout()).renderRows(cmd.OutOrStdout(), []issueRow{{
					ID: issueRef, Title: selected.Title, Owner: owner,
					Priority: selected.Priority, Status: "open",
				}})
			}
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "select a ready issue across all non-archived projects")
	cmd.Flags().BoolVar(&unowned, "unowned", false, "only issues with no owner")
	cmd.Flags().StringVar(&owner, "owner", "", "only issues owned by this actor")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "only issues with this label (repeatable, AND logic)")
	cmd.Flags().StringSliceVar(&noLabels, "no-label", nil, "exclude issues with this label (repeatable)")
	cmd.Flags().BoolVar(&full, "full", false, "show full issue details")
	return cmd
}

func writeNextAgent(w io.Writer, issueRef string, issue readyIssueForCLI) error {
	if _, err := fmt.Fprintf(w, "OK next issue=%s", agentValue(issueRef)); err != nil {
		return err
	}
	if issue.Priority != nil {
		if _, err := fmt.Fprintf(w, " priority=%d", *issue.Priority); err != nil {
			return err
		}
	}
	if issue.Owner != nil && *issue.Owner != "" {
		if _, err := fmt.Fprintf(w, " owner=%s", agentValue(*issue.Owner)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, " title=%s\n", agentValue(issue.Title))
	return err
}
