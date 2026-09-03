package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	"golang.org/x/text/unicode/norm"
)

// newSearchCmd returns the cobra.Command for `kata search`. It calls the
// daemon's GET /search endpoint and prints either the JSON envelope (under
// --json) or one line per hit with short_id, score, status, title, and match fields.
func newSearchCmd() *cobra.Command {
	var limit int
	var includeDeleted bool
	var lexical, hybrid, semantic bool
	var labels, noLabels []string
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
			if len(labels) > 0 || len(noLabels) > 0 {
				if err := requireDaemonAPIVersion(ctx, client, baseURL,
					apiVersionReadyAndSearchFilters, "filtered search"); err != nil {
					return err
				}
			}
			searchURL := buildSearchURL(searchURLParams{
				BaseURL:        baseURL,
				PID:            pid,
				Query:          query,
				Limit:          limit,
				IncludeDeleted: includeDeleted,
				Mode:           mode,
				Labels:         labels,
				NoLabels:       noLabels,
			})
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
	cmd.Flags().StringSliceVar(&labels, "label", nil, "only issues with this label (repeatable, AND logic)")
	cmd.Flags().StringSliceVar(&noLabels, "no-label", nil, "exclude issues with this label (repeatable)")
	return cmd
}

// searchURLParams bundles buildSearchURL's inputs; the daemon URL, project ID,
// query, and search options together push past the repo's five-positional-
// param convention, so they're collected here instead of passed individually.
type searchURLParams struct {
	BaseURL        string
	PID            int64
	Query          string
	Limit          int
	IncludeDeleted bool
	Mode           string
	Labels         []string
	NoLabels       []string
}

// buildSearchURL assembles the GET /search request URL with q, optional limit,
// optional include_deleted, optional mode, and repeated label/exclude_label
// query params.
func buildSearchURL(p searchURLParams) string {
	q := url.Values{}
	q.Set("q", p.Query)
	if p.Limit > 0 {
		q.Set("limit", fmt.Sprint(p.Limit))
	}
	if p.IncludeDeleted {
		q.Set("include_deleted", "true")
	}
	if p.Mode != "" {
		q.Set("mode", p.Mode)
	}
	for _, l := range p.Labels {
		q.Add("label", l)
	}
	for _, l := range p.NoLabels {
		q.Add("exclude_label", l)
	}
	return fmt.Sprintf("%s/api/v1/projects/%d/search?%s", p.BaseURL, p.PID, q.Encode())
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
				ShortID  string  `json:"short_id"`
				Title    string  `json:"title"`
				Body     string  `json:"body"`
				Status   string  `json:"status"`
				Owner    *string `json:"owner"`
				Priority *int64  `json:"priority"`
				Revision int64   `json:"revision"`
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
			excerpt := searchAgentExcerpt(b.Query, r.Issue.Body)
			fields := []agentField{
				agentRowField("issue", r.Issue.ShortID),
				agentRowFloatField("score", r.Score),
				agentRowField("status", r.Issue.Status),
				agentRowListField("matched", r.MatchedIn),
				agentRowField("title", r.Issue.Title),
				agentOptionalRowField("owner", r.Issue.Owner),
				agentRowIntField("priority", r.Issue.Priority),
				agentRowField("revision", fmt.Sprint(r.Issue.Revision)),
				agentOptionalRowField("excerpt", &excerpt),
			}
			if err := writeAgentKVRow(out, fields...); err != nil {
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

const agentSearchExcerptLimit = 160

type searchExcerptToken struct {
	value      string
	runeOffset int
	runeLength int
}

func searchAgentExcerpt(query, body string) string {
	words := strings.Fields(body)
	if len(words) == 0 {
		return ""
	}
	queryTokens := tokenizeSearchExcerpt(query)
	hit := 0
	matchOffset := 0
	matchLength := 0
	found := false
	for i, word := range words {
		for _, candidate := range tokenizeSearchExcerpt(word) {
			for _, queryToken := range queryTokens {
				if candidate.value == queryToken.value {
					hit = i
					matchOffset = candidate.runeOffset
					matchLength = candidate.runeLength
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			break
		}
	}
	start := 0
	if found {
		start = max(0, hit-8)
	}
	end := min(len(words), start+24)
	excerptRunes := []rune(strings.Join(words[start:end], " "))
	focusStart := -1
	if found {
		focusStart = matchOffset
		for i := start; i < hit; i++ {
			focusStart += utf8.RuneCountInString(words[i]) + 1
		}
	}
	leftTrimmed := start > 0
	rightTrimmed := end < len(words)
	markerRunes := 0
	if leftTrimmed {
		markerRunes += 2
	}
	if rightTrimmed {
		markerRunes += 2
	}
	if len(excerptRunes)+markerRunes <= agentSearchExcerptLimit {
		return formatSearchExcerpt(excerptRunes, leftTrimmed, rightTrimmed)
	}

	// Reserve room for both ellipsis markers. Center the bounded window on
	// the matched query term so long context before it cannot hide the match.
	windowSize := agentSearchExcerptLimit - 4
	windowStart := 0
	if focusStart >= 0 {
		focusEnd := focusStart + matchLength
		windowStart = max(0, (focusStart+focusEnd-windowSize)/2)
		windowStart = min(windowStart, max(0, len(excerptRunes)-windowSize))
	}
	windowEnd := min(len(excerptRunes), windowStart+windowSize)
	return formatSearchExcerpt(
		excerptRunes[windowStart:windowEnd],
		leftTrimmed || windowStart > 0,
		rightTrimmed || windowEnd < len(excerptRunes),
	)
}

func tokenizeSearchExcerpt(value string) []searchExcerptToken {
	runes := []rune(value)
	tokens := make([]searchExcerptToken, 0, len(strings.Fields(value)))
	for i := 0; i < len(runes); {
		if !isSearchExcerptTokenRune(runes[i]) {
			i++
			continue
		}
		start := i
		for i < len(runes) && isSearchExcerptTokenRune(runes[i]) {
			i++
		}
		tokens = append(tokens, searchExcerptToken{
			value:      foldSearchExcerptToken(string(runes[start:i])),
			runeOffset: start,
			runeLength: i - start,
		})
	}
	return tokens
}

// foldSearchExcerptToken lowercases and strips combining marks so excerpt
// matching folds diacritics the way both lexical search backends do.
func foldSearchExcerptToken(value string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(value) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func isSearchExcerptTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r)
}

func formatSearchExcerpt(runes []rune, leftTrimmed, rightTrimmed bool) string {
	excerpt := string(runes)
	if leftTrimmed {
		excerpt = "… " + excerpt
	}
	if rightTrimmed {
		excerpt += " …"
	}
	return excerpt
}
