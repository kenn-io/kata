package main

import (
	"bytes"
	"encoding/json"
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
