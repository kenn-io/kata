package main

import (
	"bytes"
	"fmt"

	"github.com/spf13/cobra"
)

func newReadyCmd() *cobra.Command {
	var (
		limit    int
		all      bool
		unowned  bool
		owner    string
		labels   []string
		noLabels []string
	)
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "list open issues with no open blocks predecessor",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options := readyOptions{
				Limit: limit, All: all, Unowned: unowned, Owner: owner,
				Labels: labels, NoLabels: noLabels,
			}
			result, err := options.fetch(cmd)
			if err != nil {
				return err
			}
			mode := currentOutputMode()
			if mode == outputJSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, result.Raw); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}

			if all {
				if mode == outputAgent {
					out := cmd.OutOrStdout()
					if _, err := fmt.Fprintf(out, "OK ready count=%d\n", len(result.Issues)); err != nil {
						return err
					}
					for _, i := range result.Issues {
						if err := writeAgentKVRow(out,
							agentRowField("issue", i.ProjectName+"#"+i.ShortID),
							agentRowIntField("priority", i.Priority),
							agentOptionalRowField("owner", i.Owner),
							agentRowField("title", i.Title),
						); err != nil {
							return err
						}
					}
					return nil
				}
				rows := make([]issueRow, len(result.Issues))
				for idx, i := range result.Issues {
					owner := "-"
					if i.Owner != nil {
						owner = *i.Owner
					}
					rows[idx] = issueRow{
						ID:       i.ProjectName + "#" + i.ShortID,
						Title:    i.Title,
						Owner:    owner,
						Priority: i.Priority,
						Status:   "open",
					}
				}
				renderer := newRowRenderer(cmd.OutOrStdout())
				if err := renderer.renderRows(cmd.OutOrStdout(), rows); err != nil {
					return err
				}
				// ready's default limit is 0 (no limit); a positive limit
				// that returned exactly that many rows may be truncating.
				truncated := limit > 0 && len(rows) == limit
				if !flags.Quiet && len(rows) > 0 {
					if err := renderer.renderReadyFooter(cmd.OutOrStdout(), len(rows), truncated); err != nil {
						return err
					}
				}
				return nil
			}

			if mode == outputAgent {
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "OK ready count=%d\n", len(result.Issues)); err != nil {
					return err
				}
				for _, i := range result.Issues {
					if err := writeAgentKVRow(out,
						agentRowField("issue", i.ShortID),
						agentRowIntField("priority", i.Priority),
						agentOptionalRowField("owner", i.Owner),
						agentRowField("title", i.Title),
					); err != nil {
						return err
					}
				}
				return nil
			}
			rows := make([]issueRow, len(result.Issues))
			for idx, i := range result.Issues {
				ownerStr := "-"
				if i.Owner != nil {
					ownerStr = *i.Owner
				}
				rows[idx] = issueRow{
					ID:       i.ShortID,
					Title:    i.Title,
					Owner:    ownerStr,
					Priority: i.Priority,
					Status:   "open",
				}
			}
			renderer := newRowRenderer(cmd.OutOrStdout())
			if err := renderer.renderRows(cmd.OutOrStdout(), rows); err != nil {
				return err
			}
			// ready's default limit is 0 (no limit); a positive limit
			// that returned exactly that many rows may be truncating.
			truncated := limit > 0 && len(rows) == limit
			if !flags.Quiet && len(rows) > 0 {
				if err := renderer.renderReadyFooter(cmd.OutOrStdout(), len(rows), truncated); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = no limit)")
	cmd.Flags().BoolVar(&all, "all", false, "list ready issues across all non-archived projects")
	cmd.Flags().BoolVar(&unowned, "unowned", false, "only issues with no owner")
	cmd.Flags().StringVar(&owner, "owner", "", "only issues owned by this actor")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "only issues with this label (repeatable, AND logic)")
	cmd.Flags().StringSliceVar(&noLabels, "no-label", nil, "exclude issues with this label (repeatable)")
	return cmd
}
