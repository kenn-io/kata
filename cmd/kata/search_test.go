package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderSearch drives printSearchResults directly: it sets the active output
// mode, feeds the raw response body, and returns captured stdout. It isolates
// the rendering contract (header rule, score precision, agent field order) from
// daemon round-trips, mirroring the cobra output-capture pattern used by the
// other cmd/kata tests.
func renderSearch(t *testing.T, mode outputMode, body string) string {
	t.Helper()
	resetFlags(t)
	flags.Mode = mode
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	require.NoError(t, printSearchResults(cmd, []byte(body)))
	return buf.String()
}

// TestSearch_OutputsShortIDNotNumber pins the JSON wire shape: each search
// result's nested issue carries short_id; the legacy `number` field is gone.
func TestSearch_OutputsShortIDNotNumber(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "matchable title")

	out, err := runCmdOutput(t, env, "--workspace", dir, "--json", "search", "matchable")
	require.NoError(t, err)
	var got struct {
		Results []struct {
			Issue map[string]any `json:"issue"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotEmpty(t, got.Results)
	issue := got.Results[0].Issue
	_, hasShort := issue["short_id"]
	_, hasNumber := issue["number"]
	assert.True(t, hasShort, "short_id missing from search hit: %v", issue)
	assert.False(t, hasNumber, "number still present in search hit: %v", issue)
}

func TestSearch_ReturnsMatchedIssues(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "fix login crash on Safari")
	createIssue(t, env, pid, "unrelated issue")

	out := runCLI(t, env, dir, "search", "login Safari")
	assert.Contains(t, out, "fix login crash on Safari")
	assert.NotContains(t, out, "unrelated issue")
}

func TestSearch_AgentOutputEmptyEmitsOnlyHeader(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	out, stderr, err := runCLIWithErr(t, env, dir, "--agent", "search", "login race")

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "OK search count=0 query=\"login race\" mode=lexical\n", out)
}

func TestSearch_EmptyQueryIsValidationError(t *testing.T) {
	f := newCLIFixture(t)
	_ = requireCLIError(t, f.execute("search", "  "), ExitValidation)
}

// TestSearch_UnquotedMultiTerm verifies that `kata search login Safari`
// (no quotes) joins the args with spaces and matches the same way as the
// quoted form. Required by the BM25 implicit-AND contract.
func TestSearch_UnquotedMultiTerm(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "fix login crash on Safari")
	createIssue(t, env, pid, "unrelated issue")

	out := runCLI(t, env, dir, "search", "login", "Safari")
	assert.Contains(t, out, "fix login crash on Safari")
	assert.NotContains(t, out, "unrelated issue")
}

// TestSearchHumanBaselineLexicalUnchanged pins that plain lexical output (the
// unconfigured daemon, auto-with-embeddings-off, and explicit --lexical) stays
// byte-identical to today: no mode header, %.2f scores.
func TestSearchHumanBaselineLexicalUnchanged(t *testing.T) {
	body := `{"query":"login","mode":"lexical","results":[
	  {"issue":{"short_id":"abc4","title":"Fix login","status":"open"},"score":1.23,"matched_in":["title"]}]}`
	out := renderSearch(t, outputHuman, body)
	if strings.Contains(out, "# mode=") {
		t.Fatalf("baseline lexical must not print a mode header:\n%s", out)
	}
	if !strings.Contains(out, "abc4") || !strings.Contains(out, "1.23") {
		t.Fatalf("row format changed:\n%s", out)
	}
}

// TestSearchHumanDegradedAutoPrintsNote pins that a degraded lexical result
// (auto fell back because the embedder is down) still prints a labeled note so
// the human knows semantic results are missing.
func TestSearchHumanDegradedAutoPrintsNote(t *testing.T) {
	body := `{"query":"login","mode":"lexical","degraded":true,"degraded_reason":"embedder unreachable","results":[]}`
	out := renderSearch(t, outputHuman, body)
	if !strings.Contains(out, "# mode=lexical") || !strings.Contains(out, "degraded") {
		t.Fatalf("degraded auto must print a note:\n%s", out)
	}
}

// TestSearchHumanHybridUsesHigherPrecision pins the %.4f precision for hybrid
// (and semantic) scores, which cluster around 0.01-0.03 and would flatten
// under %.2f, plus the mode header.
func TestSearchHumanHybridUsesHigherPrecision(t *testing.T) {
	body := `{"query":"login","mode":"hybrid","results":[
	  {"issue":{"short_id":"abc4","title":"Fix login","status":"open"},"score":0.0163,"matched_in":["title","semantic"]}]}`
	out := renderSearch(t, outputHuman, body)
	if !strings.Contains(out, "# mode=hybrid") {
		t.Fatalf("hybrid must print mode header:\n%s", out)
	}
	if !strings.Contains(out, "0.0163") {
		t.Fatalf("hybrid must use %%.4f precision:\n%s", out)
	}
}

// TestSearchAgentAppendsMode pins the appended agent field order: mode= follows
// count= and query= without disturbing their names or positions.
func TestSearchAgentAppendsMode(t *testing.T) {
	body := `{"query":"login","mode":"lexical","results":[
	  {"issue":{"short_id":"abc4","title":"Fix login","status":"open"},"score":1.2,"matched_in":["title"]}]}`
	out := renderSearch(t, outputAgent, body)
	if !strings.Contains(out, "OK search count=1 query=login mode=lexical") {
		t.Fatalf("agent header order wrong:\n%s", out)
	}
}

func TestSearchAgentIncludesBoundedTaskContext(t *testing.T) {
	body := `{"query":"needle","mode":"lexical","results":[
	  {"issue":{"short_id":"abc4","title":"Investigate the worker","body":"zero one two three four five six seven eight nine ten eleven twelve thirteen fourteen needle nearby context sixteen seventeen eighteen nineteen twenty twenty-one twenty-two twenty-three twenty-four twenty-five twenty-six twenty-seven twenty-eight twenty-nine thirty thirty-one thirty-two thirty-three distant-tail","status":"open","owner":"alice","priority":2,"revision":7},"score":1.2,"matched_in":["body"]}]}`
	out := renderSearch(t, outputAgent, body)

	assert.Contains(t, out, "issue=abc4")
	assert.Contains(t, out, `title="Investigate the worker"`)
	assert.Contains(t, out, "status=open")
	assert.Contains(t, out, "owner=alice")
	assert.Contains(t, out, "priority=2")
	assert.Contains(t, out, "revision=7")
	assert.Contains(t, out, "needle nearby context")
	assert.NotContains(t, out, "distant-tail")
	assert.NotContains(t, out, "body=")
}

func TestSearchAgentExcerptKeepsMatchAfterLongContext(t *testing.T) {
	prefix := strings.Repeat("supercalifragilisticexpialidociousword ", 8)
	excerpt := searchAgentExcerpt("needle", prefix+"needle useful context after the match")

	assert.Contains(t, excerpt, "needle")
	assert.LessOrEqual(t, len([]rune(excerpt)), agentSearchExcerptLimit)
}

func TestSearchAgentExcerptSplitsQueryPunctuationLikeSearch(t *testing.T) {
	prefix := strings.Repeat("prefix ", 30)
	excerpt := searchAgentExcerpt("foo-bar", prefix+"foo bar useful context")

	assert.Contains(t, excerpt, "foo bar useful context")
}

func TestSearchAgentExcerptDoesNotMatchInsideAnotherToken(t *testing.T) {
	body := "catalog " + strings.Repeat("filler ", 30) + "log useful context"
	excerpt := searchAgentExcerpt("log", body)

	assert.Contains(t, excerpt, "log useful context")
}

// TestSearch_ModeFlagsMutuallyExclusive pins that --lexical/--hybrid/--semantic
// cannot be combined; each conflicting pair is a validation error.
func TestSearch_ModeFlagsMutuallyExclusive(t *testing.T) {
	pairs := [][]string{
		{"--lexical", "--hybrid"},
		{"--lexical", "--semantic"},
		{"--hybrid", "--semantic"},
	}
	for _, p := range pairs {
		args := append([]string{"search", "x"}, p...)
		_, err := runCmdOutput(t, nil, args...)
		_ = requireCLIError(t, err, ExitValidation)
	}
}

// TestSearchHumanDegradedLexicalKeepsTwoDecimals pins that degraded-lexical
// results (BM25 scores) keep %.2f — only hybrid/semantic use %.4f.
func TestSearchHumanDegradedLexicalKeepsTwoDecimals(t *testing.T) {
	body := `{"query":"login","mode":"lexical","degraded":true,"degraded_reason":"embedder unreachable","results":[
	  {"issue":{"short_id":"abc4","title":"Fix login","status":"open"},"score":2.5,"matched_in":["title"]}]}`
	out := renderSearch(t, outputHuman, body)
	if !strings.Contains(out, "2.50") || strings.Contains(out, "2.5000") {
		t.Fatalf("degraded-lexical must use %%.2f, not %%.4f:\n%s", out)
	}
}

// TestSearchHumanOldDaemonEmptyModeRendersAsLexical pins that a response with
// no "mode" field (a pre-0.3.0 daemon, reachable only in remote-client mode)
// renders as the lexical baseline rather than a bare "# mode=" line.
func TestSearchHumanOldDaemonEmptyModeRendersAsLexical(t *testing.T) {
	body := `{"query":"login","results":[
	  {"issue":{"short_id":"abc4","title":"Fix login","status":"open"},"score":1.23,"matched_in":["title"]}]}`
	out := renderSearch(t, outputHuman, body)
	if strings.Contains(out, "# mode=") {
		t.Fatalf("empty mode must not print a header:\n%s", out)
	}
	if !strings.Contains(out, "1.23") {
		t.Fatalf("empty mode must render lexical %%.2f rows:\n%s", out)
	}
}

// TestSearch_RepeatedLabelFiltersRequireEveryLabel mirrors
// TestList_RepeatedLabelFiltersRequireEveryLabel: repeated --label flags on
// `kata search` must AND together, matching only issues carrying every
// named label.
func TestSearch_RepeatedLabelFiltersRequireEveryLabel(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	alphaOnly := createIssue(t, env, pid, "sprocket alpha only")
	alphaBeta := createIssue(t, env, pid, "sprocket alpha beta")
	runCLI(t, env, dir, "label", "add", alphaOnly, "alpha")
	runCLI(t, env, dir, "label", "add", alphaBeta, "alpha")
	runCLI(t, env, dir, "label", "add", alphaBeta, "beta")

	out := runCLI(t, env, dir, "search", "sprocket", "--label", "alpha", "--label", "beta")
	assert.Contains(t, out, "sprocket alpha beta")
	assert.NotContains(t, out, "sprocket alpha only")
}

// TestSearch_RepeatedNoLabelFiltersExcludeEveryLabel mirrors
// TestList_RepeatedNoLabelFiltersExcludeEveryLabel: repeated --no-label
// flags on `kata search` exclude any issue carrying any named label.
func TestSearch_RepeatedNoLabelFiltersExcludeEveryLabel(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	alpha := createIssue(t, env, pid, "widget alpha candidate")
	beta := createIssue(t, env, pid, "widget beta candidate")
	createIssue(t, env, pid, "widget plain candidate")
	runCLI(t, env, dir, "label", "add", alpha, "alpha")
	runCLI(t, env, dir, "label", "add", beta, "beta")

	out := runCLI(t, env, dir, "search", "widget", "--no-label", "alpha", "--no-label", "beta")
	assert.Contains(t, out, "widget plain candidate")
	assert.NotContains(t, out, "widget alpha candidate")
	assert.NotContains(t, out, "widget beta candidate")
}

// TestBuildSearchURLEncodesLabelFilters verifies buildSearchURL emits a
// repeated "label" param per --label value and a repeated "exclude_label"
// param per --no-label value, order-insensitive.
func TestBuildSearchURLEncodesLabelFilters(t *testing.T) {
	got := buildSearchURL(searchURLParams{
		BaseURL:  "http://example.test",
		PID:      1,
		Query:    "q",
		Limit:    20,
		Labels:   []string{"bug", "urgent"},
		NoLabels: []string{"wip"},
	})
	idx := strings.Index(got, "?")
	require.NotEqual(t, -1, idx, "expected a query string: %s", got)
	values, err := url.ParseQuery(got[idx+1:])
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"bug", "urgent"}, values["label"])
	assert.ElementsMatch(t, []string{"wip"}, values["exclude_label"])
}

// TestSearch_RejectsNonPositiveLimit covers hammer-test #5: --limit
// 0/-1 used to be silently treated as "no limit" because
// buildSearchURL only set the param when limit > 0. Now mirrors
// list/ready/events/daemon-logs validation.
func TestSearch_RejectsNonPositiveLimit(t *testing.T) {
	for _, lim := range []string{"0", "-1"} {
		_, err := runCmdOutput(t, nil, "search", "x", "--limit", lim)
		_ = requireCLIError(t, err, ExitValidation)
	}
}
