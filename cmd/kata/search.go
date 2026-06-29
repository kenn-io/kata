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
// --json) or one line per hit with short_id, score, status, title, and match fields.
func newSearchCmd() *cobra.Command {
	var limit int
	var includeDeleted bool
	var lexical, hybrid, semantic bool
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
			modeFlags := 0
			for _, b := range []bool{lexical, hybrid, semantic} {
				if b {
					modeFlags++
				}
			}
			if modeFlags > 1 {
				return &cliError{Message: "--lexical, --hybrid, and --semantic are mutually exclusive", Kind: kindValidation, ExitCode: ExitValidation}
			}
			mode := ""
			switch {
			case lexical:
				mode = "lexical"
			case hybrid:
				mode = "hybrid"
			case semantic:
				mode = "semantic"
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
			searchURL := buildSearchURL(baseURL, pid, query, limit, includeDeleted, mode)
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
	cmd.Flags().BoolVar(&lexical, "lexical", false, "lexical (FTS) search only")
	cmd.Flags().BoolVar(&hybrid, "hybrid", false, "hybrid lexical+semantic search")
	cmd.Flags().BoolVar(&semantic, "semantic", false, "semantic (vector) search only")
	return cmd
}

// buildSearchURL assembles the GET /search request URL with q, optional limit,
// optional include_deleted, and optional mode query params.
func buildSearchURL(baseURL string, pid int64, query string, limit int, includeDeleted bool, mode string) string {
	q := url.Values{}
	q.Set("q", query)
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	if includeDeleted {
		q.Set("include_deleted", "true")
	}
	if mode != "" {
		q.Set("mode", mode)
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
		Query          string `json:"query"`
		Mode           string `json:"mode"`
		Degraded       bool   `json:"degraded"`
		DegradedReason string `json:"degraded_reason"`
		Results        []struct {
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
	// A pre-0.3.0 daemon (reachable only in remote-client mode) omits "mode";
	// it only ever did lexical search, so render it as the lexical baseline
	// rather than emitting a bare "# mode=" / "mode=" line.
	if b.Mode == "" {
		b.Mode = "lexical"
	}
	if mode == outputAgent {
		out := cmd.OutOrStdout()
		header := fmt.Sprintf("OK search count=%d query=%s mode=%s", len(b.Results), agentValue(b.Query), b.Mode)
		if b.Degraded {
			header += " degraded=" + agentValue(b.DegradedReason)
		}
		if _, err := fmt.Fprintln(out, header); err != nil {
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
	// Header rule keyed on whether this is the plain baseline, not the
	// effective mode alone: print a leading "# mode=<mode>" line whenever the
	// mode is hybrid/semantic OR the result is degraded. Baseline lexical
	// (mode=lexical, not degraded) stays byte-identical to today — no header,
	// %.2f scores — so degraded auto fallback is silent-but-labeled rather
	// than silent. See docs/design/semantic-search.md "API and CLI contract".
	if b.Mode != "lexical" || b.Degraded {
		header := "# mode=" + b.Mode
		if b.Degraded {
			header += " degraded: " + b.DegradedReason
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), header); err != nil {
			return err
		}
	}
	if len(b.Results) == 0 {
		if flags.Quiet {
			return nil
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no matches")
		return err
	}
	// RRF and cosine scores cluster around 0.01-0.03, which %.2f would flatten;
	// hybrid/semantic use %.4f. Degraded-lexical results are ordinary BM25 and
	// keep %.2f.
	scoreFmt := "%.2f"
	if b.Mode == "hybrid" || b.Mode == "semantic" {
		scoreFmt = "%.4f"
	}
	for _, r := range b.Results {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%-8s  "+scoreFmt+"  %-8s  %s  (%s)\n",
			r.Issue.ShortID, r.Score, r.Issue.Status,
			textsafe.Line(r.Issue.Title),
			strings.Join(r.MatchedIn, ",")); err != nil {
			return err
		}
	}
	return nil
}
