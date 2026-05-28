package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
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
			if limit < 0 {
				return &cliError{Message: "--limit must be non-negative", Kind: kindValidation, ExitCode: ExitValidation}
			}
			if unowned && owner != "" {
				return &cliError{Message: "--unowned and --owner are mutually exclusive", Kind: kindValidation, ExitCode: ExitValidation}
			}
			if all && strings.TrimSpace(flags.Project) != "" {
				return &cliError{
					Message:  "--project and --all are mutually exclusive",
					Kind:     kindUsage,
					ExitCode: ExitUsage,
				}
			}
			// The global ready endpoint does not apply per-project filter
			// flags. Reject combinations that would silently ignore them
			// rather than returning misleading results.
			if all && (unowned || owner != "" || len(labels) > 0 || len(noLabels) > 0) {
				return &cliError{
					Message:  "--all does not support --unowned, --owner, --label, or --no-label",
					Kind:     kindUsage,
					ExitCode: ExitUsage,
				}
			}
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}

			var getURL string
			if all {
				getURL = baseURL + "/api/v1/ready"
			} else {
				start, err := resolveStartPath(flags.Workspace)
				if err != nil {
					return err
				}
				pid, err := resolveProjectID(ctx, baseURL, start)
				if err != nil {
					return err
				}
				getURL = fmt.Sprintf("%s/api/v1/projects/%d/ready", baseURL, pid)
			}

			// Build query parameters
			params := url.Values{}
			if limit > 0 {
				params.Set("limit", fmt.Sprintf("%d", limit))
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

			// Append query string if params exist
			if len(params) > 0 {
				getURL += "?" + params.Encode()
			}

			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, getURL, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
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

			if all {
				var b struct {
					Issues []struct {
						ShortID     string  `json:"short_id"`
						Title       string  `json:"title"`
						Owner       *string `json:"owner,omitempty"`
						ProjectName string  `json:"project_name"`
					} `json:"issues"`
				}
				if err := json.Unmarshal(bs, &b); err != nil {
					return err
				}
				for _, i := range b.Issues {
					owner := "-"
					if i.Owner != nil {
						owner = *i.Owner
					}
					qualified := i.ProjectName + "#" + i.ShortID
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-16s  %s  (%s)\n",
						qualified, textsafe.Line(i.Title), textsafe.Line(owner)); err != nil {
						return err
					}
				}
				return nil
			}

			var b struct {
				Issues []struct {
					ShortID  string  `json:"short_id"`
					Title    string  `json:"title"`
					Owner    *string `json:"owner,omitempty"`
					Priority *int64  `json:"priority"`
				} `json:"issues"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
			}
			if mode == outputAgent {
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprintf(out, "OK ready count=%d\n", len(b.Issues)); err != nil {
					return err
				}
				for _, i := range b.Issues {
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
			for _, i := range b.Issues {
				ownerStr := "-"
				if i.Owner != nil {
					ownerStr = *i.Owner
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-8s  %s  (%s)\n",
					i.ShortID, textsafe.Line(i.Title), textsafe.Line(ownerStr)); err != nil {
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
