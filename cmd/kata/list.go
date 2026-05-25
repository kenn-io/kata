package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

func newListCmd() *cobra.Command {
	var status string
	var limit int
	var priority int
	var maxPriority int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list issues in this project",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit <= 0 {
				return &cliError{Message: "--limit must be a positive integer", Kind: kindValidation, ExitCode: ExitValidation}
			}
			if cmd.Flags().Changed("priority") && (priority < 0 || priority > 4) {
				return &cliError{Message: "--priority must be between 0 and 4", Kind: kindValidation, ExitCode: ExitValidation}
			}
			if cmd.Flags().Changed("max-priority") && (maxPriority < 0 || maxPriority > 4) {
				return &cliError{Message: "--max-priority must be between 0 and 4", Kind: kindValidation, ExitCode: ExitValidation}
			}
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
			// "all" is a CLI sentinel meaning "no filter"; the server expects
			// an empty status to return both open and closed.
			apiStatus := status
			if apiStatus == "all" {
				apiStatus = ""
			}
			url := fmt.Sprintf("%s/api/v1/projects/%d/issues?status=%s&limit=%d", baseURL, pid, apiStatus, limit)
			if cmd.Flags().Changed("priority") {
				url += fmt.Sprintf("&priority=%d", priority)
			}
			if cmd.Flags().Changed("max-priority") {
				url += fmt.Sprintf("&max_priority=%d", maxPriority)
			}
			httpStatus, bs, err := httpDoJSON(ctx, client, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			if httpStatus >= 400 {
				return apiErrFromBody(httpStatus, bs)
			}
			if flags.JSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			var b struct {
				Issues []struct {
					ShortID     string  `json:"short_id"`
					QualifiedID string  `json:"qualified_id"`
					Title       string  `json:"title"`
					Status      string  `json:"status"`
					Owner       *string `json:"owner"`
				} `json:"issues"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			// Show owner in parens to match ready's convention. Owner
			// is the actionable identity ("who's responsible") whereas
			// author is historical metadata; mixing the two between
			// list and ready confused users (hammer-test finding #10).
			// Unowned issues render as "(unowned)" so the trailing
			// "(...)" cell is never empty.
			for _, i := range b.Issues {
				owner := "unowned"
				if i.Owner != nil && *i.Owner != "" {
					owner = *i.Owner
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-8s  %-8s  %s  (%s)\n",
					i.ShortID, i.Status,
					textsafe.Line(i.Title), textsafe.Line(owner)); err != nil {
					return err
				}
			}
			// Truncation hint: when we got exactly --limit rows back the
			// daemon may have more. Print to stderr so pipelines stay
			// clean (kata list | grep ...). Quiet suppresses it. Has a
			// false positive when the project has exactly --limit issues,
			// which we accept as a much smaller harm than silent
			// truncation on projects above the default.
			if !flags.Quiet && len(b.Issues) == limit {
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(),
					"... showing %d (raise --limit to see more)\n", limit); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "open", "filter by status: open|closed|all")
	cmd.Flags().IntVar(&limit, "limit", 200, "max rows")
	cmd.Flags().IntVar(&priority, "priority", 0, "exact priority filter (0..4); 0 = highest")
	cmd.Flags().IntVar(&maxPriority, "max-priority", 0, "include only priority <= this value (0..4)")
	return cmd
}
