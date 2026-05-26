package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/api"
)

func newAuditClosesCmd() *cobra.Command {
	var (
		since      string
		until      string
		actor      string
		parent     string
		reason     string
		noEvidence bool
	)
	cmd := &cobra.Command{
		Use:   "closes",
		Short: "list close events, with filters",
		Long: `kata audit closes prints one row per issue.closed event in the
current project. Filter by actor, reason, time window, parent, or
"no-evidence" to spot agents closing en masse, closing without
evidence, or reusing the same prose across siblings.

The default JSON shape is stable; pipe it through jq for ad-hoc
analysis. The text output is a wide table; pass --json for tooling.`,
		Args: cobra.NoArgs,
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
			q := url.Values{}
			q.Set("project_id", fmt.Sprintf("%d", pid))
			if since != "" {
				q.Set("since", since)
			}
			if until != "" {
				q.Set("until", until)
			}
			if actor != "" {
				q.Set("actor", actor)
			}
			if parent != "" {
				q.Set("parent", parent)
			}
			if reason != "" {
				q.Set("reason", reason)
			}
			if noEvidence {
				q.Set("no_evidence", "true")
			}
			getURL := fmt.Sprintf("%s/api/v1/audit/closes?%s", baseURL, q.Encode())
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
			if mode == outputAgent {
				return printAuditClosesAgent(cmd, bs)
			}
			return printAuditClosesTable(cmd, bs)
		},
	}
	cmd.Flags().StringVar(&since, "since", "",
		"RFC3339 timestamp; default = beginning of time")
	cmd.Flags().StringVar(&until, "until", "",
		"RFC3339 timestamp; default = now")
	cmd.Flags().StringVar(&actor, "actor", "", "filter by actor")
	cmd.Flags().StringVar(&parent, "parent", "",
		"filter to closes of children of this parent ref")
	cmd.Flags().StringVar(&reason, "reason", "",
		"filter by close reason (done|wontfix|duplicate|superseded|audit-no-change)")
	cmd.Flags().BoolVar(&noEvidence, "no-evidence", false,
		"only closes flagged no-evidence (zero evidence items, non-wontfix)")
	return cmd
}

func printAuditClosesTable(cmd *cobra.Command, bs []byte) error {
	// huma serializes the operation's `Body` content directly onto the
	// wire, so the response shape is {"rows":[...]} — not the doubly
	// wrapped {"body":{"rows":[...]}} that unmarshalling into the full
	// api.AuditClosesResponse struct would expect.
	var resp struct {
		Rows []api.AuditCloseRow `json:"rows"`
	}
	if err := json.Unmarshal(bs, &resp); err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(w, "%-20s  %-12s  %-6s  %-7s  %-15s  %-15s  %s\n",
		"TIME", "ACTOR", "ISSUE", "PARENT", "REASON", "EVIDENCE", "FLAGS"); err != nil {
		return err
	}
	for _, r := range resp.Rows {
		parent := "-"
		if r.Parent != "" {
			parent = r.Parent
		}
		flagsCol := strings.Join(r.Flags, ",")
		for _, f := range r.Flags {
			if f == "throttled" || f == "rapid-burst" {
				flagsCol = "!! " + flagsCol
				break
			}
		}
		if _, err := fmt.Fprintf(w, "%-20s  %-12s  %-6s  %-7s  %-15s  %-15s  %s\n",
			r.Time, r.Actor, r.Issue, parent, r.Reason,
			strings.Join(r.EvidenceTypes, ","),
			flagsCol); err != nil {
			return err
		}
	}
	return nil
}

func printAuditClosesAgent(cmd *cobra.Command, bs []byte) error {
	var resp struct {
		Rows []api.AuditCloseRow `json:"rows"`
	}
	if err := json.Unmarshal(bs, &resp); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "OK audit count=%d\n", len(resp.Rows)); err != nil {
		return err
	}
	for _, r := range resp.Rows {
		fields := []agentField{
			agentRowField("time", r.Time),
			agentRowField("actor", r.Actor),
			agentRowField("issue", r.Issue),
		}
		if r.Parent != "" {
			fields = append(fields, agentRowField("parent", r.Parent))
		}
		fields = append(fields,
			agentRowField("reason", r.Reason),
			agentRowListField("evidence", r.EvidenceTypes),
			agentRowListField("flags", r.Flags),
		)
		if err := writeAgentKVRow(out, fields...); err != nil {
			return err
		}
	}
	return nil
}
