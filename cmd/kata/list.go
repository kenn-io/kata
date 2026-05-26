package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

func newListCmd() *cobra.Command {
	var status string
	var limit int
	var priority int
	var maxPriority int
	var unowned bool
	var owner string
	var labels []string
	var noLabels []string
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
			if unowned && owner != "" {
				return &cliError{Message: "--unowned and --owner are mutually exclusive", Kind: kindValidation, ExitCode: ExitValidation}
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
			// Build base URL
			getURL := fmt.Sprintf("%s/api/v1/projects/%d/issues", baseURL, pid)

			// Build query parameters
			params := url.Values{}
			params.Set("status", apiStatus)
			params.Set("limit", fmt.Sprintf("%d", limit))
			if cmd.Flags().Changed("priority") {
				params.Set("priority", fmt.Sprintf("%d", priority))
			}
			if cmd.Flags().Changed("max-priority") {
				params.Set("max_priority", fmt.Sprintf("%d", maxPriority))
			}
			if unowned {
				params.Set("unowned", "true")
			}
			if owner != "" {
				params.Set("owner", owner)
			}
			for _, l := range labels {
				params.Add("label", l)
			}
			for _, l := range noLabels {
				params.Add("exclude_label", l)
			}

			// Append query string
			getURL += "?" + params.Encode()
			httpStatus, bs, err := httpDoJSON(ctx, client, http.MethodGet, getURL, nil)
			if err != nil {
				return err
			}
			if httpStatus >= 400 {
				return apiErrFromBody(httpStatus, bs)
			}
			mode := currentOutputMode()
			if mode == outputJSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			var b struct {
				Issues []struct {
					ShortID     string   `json:"short_id"`
					QualifiedID string   `json:"qualified_id"`
					Title       string   `json:"title"`
					Status      string   `json:"status"`
					Owner       *string  `json:"owner"`
					Priority    *int64   `json:"priority"`
					Labels      []string `json:"labels"`
				} `json:"issues"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			if mode == outputAgent {
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "OK list count=%d\n", len(b.Issues)); err != nil {
					return err
				}
				for _, i := range b.Issues {
					if err := writeAgentKVRow(out,
						agentRowField("issue", i.ShortID),
						agentRowField("status", i.Status),
						agentRowIntField("priority", i.Priority),
						agentOptionalRowField("owner", i.Owner),
						agentRowListField("labels", i.Labels),
						agentRowField("title", i.Title),
					); err != nil {
						return err
					}
				}
				return nil
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
	cmd.Flags().BoolVar(&unowned, "unowned", false, "only issues with no owner")
	cmd.Flags().StringVar(&owner, "owner", "", "only issues owned by this actor")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "only issues with this label (repeatable, AND logic)")
	cmd.Flags().StringSliceVar(&noLabels, "no-label", nil, "exclude issues with this label (repeatable)")
	return cmd
}

type agentField struct {
	name  string
	value *string
}

func agentRowField(name, value string) agentField {
	return agentField{name: name, value: &value}
}

func agentOptionalRowField(name string, value *string) agentField {
	if value == nil || *value == "" {
		return agentField{name: name}
	}
	return agentField{name: name, value: value}
}

func agentRowIntField(name string, value *int64) agentField {
	if value == nil {
		return agentField{name: name}
	}
	s := fmt.Sprint(*value)
	return agentField{name: name, value: &s}
}

func agentRowFloatField(name string, value float64) agentField {
	s := fmt.Sprintf("%.2f", value)
	return agentField{name: name, value: &s}
}

func agentRowListField(name string, values []string) agentField {
	if len(values) == 0 {
		return agentField{name: name}
	}
	s := strings.Join(values, ",")
	return agentField{name: name, value: &s}
}

func writeAgentKVRow(w io.Writer, fields ...agentField) error {
	if _, err := fmt.Fprint(w, "-"); err != nil {
		return err
	}
	for _, f := range fields {
		if f.value == nil {
			continue
		}
		if _, err := fmt.Fprintf(w, " %s=%s", f.name, agentValue(*f.value)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}
