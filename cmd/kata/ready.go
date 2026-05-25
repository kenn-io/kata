package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

func newReadyCmd() *cobra.Command {
	var limit int
	var unowned bool
	var owner string
	var labels []string
	var noLabels []string

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
			// Build base URL
			getURL := fmt.Sprintf("%s/api/v1/projects/%d/ready", baseURL, pid)

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
					ShortID string  `json:"short_id"`
					Title   string  `json:"title"`
					Owner   *string `json:"owner,omitempty"`
				} `json:"issues"`
			}
			if err := json.Unmarshal(bs, &b); err != nil {
				return err
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
	cmd.Flags().BoolVar(&unowned, "unowned", false, "only issues with no owner")
	cmd.Flags().StringVar(&owner, "owner", "", "only issues owned by this actor")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "only issues with this label (repeatable, AND logic)")
	cmd.Flags().StringSliceVar(&noLabels, "no-label", nil, "exclude issues with this label (repeatable)")
	return cmd
}
