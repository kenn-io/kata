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

// newSearchCmd returns the cobra.Command for `kata search`. It calls the
// daemon's GET /search endpoint and prints either the JSON envelope (under
// --json) or one line per hit in `#N <score> <status>  <title>  (<matched_in>)`.
func newSearchCmd() *cobra.Command {
	var limit int
	var includeDeleted bool
	cmd := &cobra.Command{
		Use:   "search <query>...",
		Short: "search issues by title/body/comments",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Join unquoted args with spaces so `kata search login Safari`
			// behaves the same as `kata search "login Safari"` — the BM25
			// implicit-AND splits on whitespace anyway, and quoting every
			// multi-term query is needless friction.
			query := strings.Join(args, " ")
			if strings.TrimSpace(query) == "" {
				return &cliError{Message: "query must be non-empty", Kind: kindValidation, ExitCode: ExitValidation}
			}
			// Mirror list / ready / events validation (hammer-test
			// finding #5): --limit 0/-1 used to be silently treated
			// as "no limit" because buildSearchURL only set the param
			// when limit > 0. Reject with kindValidation so the user
			// sees what actually happened.
			if limit <= 0 {
				return &cliError{Message: "--limit must be a positive integer", Kind: kindValidation, ExitCode: ExitValidation}
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
			searchURL := buildSearchURL(baseURL, pid, query, limit, includeDeleted)
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, searchURL, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			return printSearchResults(cmd, bs)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "max rows")
	cmd.Flags().BoolVar(&includeDeleted, "include-deleted", false, "include soft-deleted issues")
	return cmd
}

// buildSearchURL assembles the GET /search request URL with q, optional limit,
// and optional include_deleted query params.
func buildSearchURL(baseURL string, pid int64, query string, limit int, includeDeleted bool) string {
	q := url.Values{}
	q.Set("q", query)
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	if includeDeleted {
		q.Set("include_deleted", "true")
	}
	return fmt.Sprintf("%s/api/v1/projects/%d/search?%s", baseURL, pid, q.Encode())
}

// printSearchResults renders a search response in the active output mode:
// JSON envelope, human-readable list, or "no matches" when empty.
func printSearchResults(cmd *cobra.Command, bs []byte) error {
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
		Query   string `json:"query"`
		Results []struct {
			Issue struct {
				ShortID string `json:"short_id"`
				Title   string `json:"title"`
				Status  string `json:"status"`
			} `json:"issue"`
			Score     float64  `json:"score"`
			MatchedIn []string `json:"matched_in"`
		} `json:"results"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	if mode == outputAgent {
		out := cmd.OutOrStdout()
		if _, err := fmt.Fprintf(out, "OK search count=%d query=%s\n", len(b.Results), agentValue(b.Query)); err != nil {
			return err
		}
		for _, r := range b.Results {
			if err := writeAgentKVRow(out,
				agentRowField("issue", r.Issue.ShortID),
				agentRowFloatField("score", r.Score),
				agentRowField("status", r.Issue.Status),
				agentRowListField("matched", r.MatchedIn),
				agentRowField("title", r.Issue.Title),
			); err != nil {
				return err
			}
		}
		return nil
	}
	if len(b.Results) == 0 {
		if flags.Quiet {
			return nil
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no matches")
		return err
	}
	for _, r := range b.Results {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-8s  %.2f  %-8s  %s  (%s)\n",
			r.Issue.ShortID, r.Score, r.Issue.Status,
			textsafe.Line(r.Issue.Title),
			strings.Join(r.MatchedIn, ",")); err != nil {
			return err
		}
	}
	return nil
}
