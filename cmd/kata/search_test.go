package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSearch_ContinuationCommandKeepsQueryAndFilters(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"inventory export first", "inventory export second", "inventory export third"} {
		ref := createIssue(t, env, pid, title)
		runCLI(t, env, dir, "label", "add", ref, "bug")
		runCLI(t, env, dir, "label", "add", ref, "ui")
		runCLI(t, env, dir, "label", "add", ref, "wont")
	}
	createIssue(t, env, pid, "inventory export distractor")
	workspace := filepath.Join(t.TempDir(), "reader's workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	t.Chdir(t.TempDir())
	ctx := contextWithBaseURL(t.Context(), env.URL)
	out, stderr, err := executeRootCapture(t, ctx, "search", "--project", "kata", "--workspace", workspace, "--lexical", "--status", "open", "--limit", "1", "--include-deleted", "--label", "bug", "--label", "ui", "--no-label", `"wont,fix"`, "--no-label", ",excluded", "--", "inventory", "--export")
	require.NoError(t, err, stderr)
	assert.Contains(t, out, "inventory export first")
	for _, title := range []string{"inventory export second", "inventory export third"} {
		_, command, ok := strings.Cut(stderr, "search results are incomplete; continue with ")
		require.True(t, ok, stderr)
		command, _, _ = strings.Cut(command, "\n")
		out, stderr, err = executeRootCapture(t, ctx, retryShellArgs(t, command)...)
		require.NoError(t, err, stderr)
		assert.Contains(t, out, title)
		assert.NotContains(t, out, "distractor")
		assert.Equal(t, 1, strings.Count(out, "inventory export"))
	}
}

func TestSearch_ContinuationRendering(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    outputMode
		quiet   bool
		body    string
		wantErr string
		wantOut string
	}{
		{"complete", outputHuman, false, `{"complete":true,"results":[]}`, "", ""},
		{"no cursor", outputHuman, false, `{"complete":false,"results":[]}`, "search results are incomplete\n", ""},
		{"quiet", outputHuman, true, `{"complete":false,"next_cursor":"next","results":[]}`, "", ""},
		{"json", outputJSON, false, `{"complete":false,"next_cursor":"next","results":[]}`, "", ""},
		{"agent", outputAgent, false, `{"complete":false,"next_cursor":"next","results":[]}`, "", "next_cursor=next"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(t)
			flags.Mode, flags.Quiet = tc.mode, tc.quiet
			cmd := newSearchCmd()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			require.NoError(t, printSearchResults(cmd, []byte(tc.body)))
			assert.Equal(t, tc.wantErr, stderr.String())
			if tc.quiet {
				assert.Empty(t, out.String())
			} else if tc.wantOut != "" {
				assert.Contains(t, out.String(), tc.wantOut)
			}
		})
	}
}

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

	out, stderr, err := runCLIWithErr(t, env, dir, "--agent", "search", "login")

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "OK search count=0 query=login mode=lexical complete=true\n", out)
}

// Lexical search returns only issues that contain every word. An agent that
// adds context words to its query gets nothing back and files a duplicate, so
// an empty multi-word search says why on stderr.
func TestSearch_EmptyMultiWordSearchHintsEveryWordMustMatch(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "paginate drops the last partial page")
	const hint = "Hint: no issue contains every word; search again with fewer words\n"

	out, stderr, err := runCLIWithErr(t, env, dir, "--agent", "search", "paginate inventory")
	require.NoError(t, err)
	assert.Equal(t, "OK search count=0 query=\"paginate inventory\" mode=lexical complete=true\n", out)
	assert.Equal(t, hint, stderr)

	out, stderr, err = runCLIWithErr(t, env, dir, "search", "paginate", "inventory")
	require.NoError(t, err)
	assert.Equal(t, "no matches\n", out)
	assert.True(t, strings.HasPrefix(stderr, hint))

	_, stderr, err = runCLIWithErr(t, env, dir, "--quiet", "search", "paginate inventory")
	require.NoError(t, err)
	assert.Empty(t, stderr, "--quiet suppresses hints")

	out, stderr, err = runCLIWithErr(t, env, dir, "--agent", "search", "paginate partial")
	require.NoError(t, err)
	assert.Contains(t, out, "count=1")
	assert.Empty(t, stderr, "a search with results needs no hint")
}

func TestSearch_EmptyQueryIsValidationError(t *testing.T) {
	f := newCLIFixture(t)
	_ = requireCLIError(t, f.execute("search", "  "), ExitValidation)
}

func TestSearch_JSONEmptyMultiWordHint(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "inventory export stalls")
	out, stderr, err := runCLIWithErr(t, env, dir, "--json", "search", "paginate inventory")
	require.NoError(t, err, stderr)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, []any{map[string]any{"code": "every_word_required", "message": "no issue contains every word; search again with fewer words"}}, got["hints"])
	assert.NotContains(t, stderr, "Hint:")
}

func TestSearch_EmptyContinuationHasNoHint(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	var ids []int64
	for _, title := range []string{"inventory export stalls", "inventory export retries"} {
		issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Author: "tester", Title: title})
		require.NoError(t, err)
		ids = append(ids, issue.ID)
	}
	out, _, err := runCLIWithErr(t, env, dir, "--json", "search", "--lexical", "--limit", "1", "inventory export")
	require.NoError(t, err)
	var first map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &first))
	require.NotEmpty(t, first["next_cursor"])
	for _, id := range ids {
		_, _, _, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: id, Title: new("unrelated"), Actor: "tester"})
		require.NoError(t, err)
	}
	out, _, err = runCLIWithErr(t, env, dir, "--json", "search", "--lexical", "--limit", "1", "--cursor", first["next_cursor"].(string), "inventory export")
	require.NoError(t, err)
	var continued map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &continued))
	assert.Empty(t, continued["results"])
	assert.NotContains(t, continued, "hints")
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

func TestSearchAgentExcerptFoldsDiacriticsLikeSearch(t *testing.T) {
	prefix := strings.Repeat("prefix ", 30)

	assert.Contains(t, searchAgentExcerpt("cafe", prefix+"café useful context"), "café useful context")
	assert.Contains(t, searchAgentExcerpt("café", prefix+"cafe useful context"), "cafe useful context")
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

// TestSearch_RejectsNonPositiveLimit covers hammer-test #5: --limit
// 0/-1 used to be silently treated as "no limit" because
// the request only set the param when limit > 0. Now mirrors
// list/ready/events/daemon-logs validation.
func TestSearch_RejectsNonPositiveLimit(t *testing.T) {
	for _, lim := range []string{"0", "-1"} {
		_, err := runCmdOutput(t, nil, "search", "x", "--limit", lim)
		_ = requireCLIError(t, err, ExitValidation)
	}
}

func TestSearchCredentialFallbackIsSilentButLabeled(t *testing.T) {
	for _, mode := range []outputMode{outputHuman, outputAgent, outputJSON} {
		t.Run(string(mode), func(t *testing.T) {
			resetFlags(t)
			flags.Mode = mode
			cmd := &cobra.Command{}
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			body := `{"query":"credential","mode":"lexical","degraded":true,"degraded_reason":"semantic search unavailable: no embedding API key (env EXAMPLE_KEY is unset)","results":[]}`
			require.NoError(t, printSearchResults(cmd, []byte(body)))
			assert.Empty(t, stderr.String())
			assert.Contains(t, out.String(), "EXAMPLE_KEY")
			if mode == outputHuman {
				assert.Contains(t, out.String(), "# mode=lexical degraded:")
			} else {
				assert.Contains(t, out.String(), "degraded")
			}
		})
	}
}

func TestSearchPlainLexicalHasNoFallbackWarning(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputHuman
	cmd := &cobra.Command{}
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	require.NoError(t, printSearchResults(cmd, []byte(`{"query":"credential","mode":"lexical","results":[]}`)))
	assert.Empty(t, stderr.String())
}

func TestSearchSuggestsSettings(t *testing.T) {
	const hint = "Search by meaning is off; turn it on in Settings (kata ui) or run kata daemon local-embeddings.\n"
	for _, tc := range []struct {
		name                                            string
		flags                                           []string
		want, configured, remote, missingHeader, cursor bool
	}{
		{name: "human", want: true},
		{name: "quiet", flags: []string{"--quiet"}},
		{name: "agent", flags: []string{"--agent"}},
		{name: "explicit", flags: []string{"--lexical"}, want: true},
		{name: "json", flags: []string{"--json"}, want: true},
		{name: "configured", flags: []string{"--lexical", "--json"}, configured: true},
		{name: "remote", flags: []string{"--json"}, remote: true},
		{name: "missing header", missingHeader: true},
		{name: "continuation", flags: []string{"--lexical"}, cursor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, dir, pid := setupCLIWorkspace(t)
			createIssue(t, env, pid, "login crash")
			if tc.missingHeader || tc.configured {
				upstream := env.URL
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream+r.URL.RequestURI(), r.Body) // #nosec G704 -- fixed loopback test daemon
					require.NoError(t, err)
					req.Header = r.Header.Clone()
					response, err := env.HTTP.Do(req) // #nosec G704 -- fixed loopback test daemon
					require.NoError(t, err)
					defer func() { _ = response.Body.Close() }()
					for name, values := range response.Header {
						if strings.EqualFold(name, "X-Kata-Embeddings-Configured") {
							continue
						}
						for _, value := range values {
							w.Header().Add(name, value)
						}
					}
					if tc.configured {
						w.Header().Set("X-Kata-Embeddings-Configured", "true")
					}
					w.WriteHeader(response.StatusCode)
					_, err = io.Copy(w, response.Body)
					require.NoError(t, err)
				}))
				t.Cleanup(proxy.Close)
				env.URL = proxy.URL
			}
			args := append([]string{"search", "login"}, tc.flags...)
			if tc.cursor {
				createIssue(t, env, pid, "login crash again")
				first, _, err := runCLIWithErr(t, env, dir, "search", "login", "--lexical", "--json", "--limit", "1")
				require.NoError(t, err)
				var page struct {
					NextCursor string `json:"next_cursor"`
				}
				require.NoError(t, json.Unmarshal([]byte(first), &page))
				require.NotEmpty(t, page.NextCursor)
				args = append(args, "--cursor", page.NextCursor)
			}
			run := func(args []string) (string, string, error) {
				if !tc.remote {
					return runCLIWithErr(t, env, dir, args...)
				}
				t.Chdir(dir)
				t.Setenv("KATA_SERVER", "")
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"), []byte(fmt.Sprintf("version = 1\n[server]\nurl = %q\n", env.URL)), 0600))
				resetFlags(t)
				cmd := newRootCmd()
				var out, stderr bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&stderr)
				cmd.SetArgs(append([]string{"--workspace", dir}, args...))
				err := cmd.ExecuteContext(t.Context())
				return out.String(), stderr.String(), err
			}
			out, stderr, err := run(args)
			require.NoError(t, err)
			require.Contains(t, out, "login crash")
			if tc.want {
				require.Equal(t, hint, stderr)
			} else {
				require.Empty(t, stderr)
			}
		})
	}
}
