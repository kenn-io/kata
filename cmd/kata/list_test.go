package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// TestList_OutputsShortIDNotNumber pins the JSON wire shape: each issue
// row carries short_id and qualified_id; the legacy `number` field is gone.
func TestList_OutputsShortIDNotNumber(t *testing.T) {
	f := newCLIFixture(t)
	createIssueViaHTTP(t, f.env, f.dir, "first")

	require.NoError(t, f.execute("--json", "list"))
	var got struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal(f.buf.Bytes(), &got))
	require.NotEmpty(t, got.Issues)
	first := got.Issues[0]
	_, hasShort := first["short_id"]
	_, hasQualified := first["qualified_id"]
	_, hasNumber := first["number"]
	assert.True(t, hasShort, "short_id missing from list row: %v", first)
	assert.True(t, hasQualified, "qualified_id missing from list row: %v", first)
	assert.False(t, hasNumber, "number still present in list row: %v", first)
}

func TestList_DefaultsToOpenIssuesInProject(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"alpha", "beta"} {
		createIssue(t, env, pid, title)
	}

	out := runCLI(t, env, dir, "list")
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "beta")
}

func TestList_AllFiltersAcrossProjectsAndUsesQualifiedRefs(t *testing.T) {
	env, _, primaryID := setupCLIWorkspace(t)
	secondary, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)

	primaryMatch := createIssue(t, env, primaryID, "primary handoff")
	secondaryMatch := createIssue(t, env, secondary.ID, "secondary handoff")
	secondaryOther := createIssue(t, env, secondary.ID, "secondary unrelated")
	for projectID, ref := range map[int64]string{
		primaryID:    primaryMatch,
		secondary.ID: secondaryMatch,
	} {
		postJSONOK(t,
			env.URL+"/api/v1/projects/"+itoa(projectID)+"/issues/"+ref+"/labels",
			map[string]any{"actor": "tester", "label": "handoff"})
	}

	out := requireCmdOutput(t, env, "--agent", "list", "--all", "--label", "HANDOFF", "--limit", "0")
	assert.Contains(t, out, "issue=kata#"+primaryMatch)
	assert.Contains(t, out, "issue=spoke-project#"+secondaryMatch)
	assert.NotContains(t, out, secondaryOther)
}

func TestList_AllJSONIncludesProjectName(t *testing.T) {
	env, _, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "global row")

	out := requireCmdOutput(t, env, "--json", "list", "--all")
	var got struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotEmpty(t, got.Issues)
	assert.Equal(t, "kata", got.Issues[0]["project_name"])
	assert.Equal(t, "kata#"+got.Issues[0]["short_id"].(string), got.Issues[0]["qualified_id"])
}

func TestList_AllDoesNotRequireWorkspaceProject(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	ref := createIssue(t, env, project.ID, "global without workspace")

	out, err := runCmdOutput(t, env, "--agent", "list", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, "issue=spoke-project#"+ref)
}

func TestList_AllAndProjectAreMutuallyExclusive(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	_, err := runCmdOutput(t, env, "--workspace", dir,
		"--project", "spoke-project", "list", "--all")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestList_LimitZeroMeansUnlimited(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"first", "second", "third"} {
		createIssue(t, env, pid, title)
	}

	out := runCLI(t, env, dir, "list", "--limit", "0")
	for _, title := range []string{"first", "second", "third"} {
		assert.Contains(t, out, title)
	}
}

func TestList_RejectsNegativeLimit(t *testing.T) {
	_, err := runCmdOutput(t, nil, "list", "--limit", "-1")
	_ = requireCLIError(t, err, ExitValidation)
}

func TestList_RepeatedLabelFiltersRequireEveryLabel(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	alphaOnly := createIssue(t, env, pid, "alpha only")
	alphaBeta := createIssue(t, env, pid, "alpha beta")
	runCLI(t, env, dir, "label", "add", alphaOnly, "alpha")
	runCLI(t, env, dir, "label", "add", alphaBeta, "alpha")
	runCLI(t, env, dir, "label", "add", alphaBeta, "beta")

	out := runCLI(t, env, dir, "list", "--label", "alpha", "--label", "beta")
	assert.Contains(t, out, "alpha beta")
	assert.NotContains(t, out, "alpha only")
}

func TestList_RepeatedNoLabelFiltersExcludeEveryLabel(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	alpha := createIssue(t, env, pid, "alpha candidate")
	beta := createIssue(t, env, pid, "beta candidate")
	createIssue(t, env, pid, "plain candidate")
	runCLI(t, env, dir, "label", "add", alpha, "alpha")
	runCLI(t, env, dir, "label", "add", beta, "beta")

	out := runCLI(t, env, dir, "list", "--no-label", "alpha", "--no-label", "beta")
	assert.Contains(t, out, "plain candidate")
	assert.NotContains(t, out, "alpha candidate")
	assert.NotContains(t, out, "beta candidate")
}

// TestList_HumanRowUsesGlyphLayout pins the new row renderer's layout: an
// open issue renders the open glyph "○ " ahead of its title, replacing the
// old "%-8s  %-8s" column format.
func TestList_HumanRowUsesGlyphLayout(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "alpha")

	out := runCLI(t, env, dir, "list")

	assert.Contains(t, out, "○ ", "expected open glyph prefix in human list output")
	assert.Contains(t, out, "alpha")
}

// TestList_HumanRowRendersPriorityChip pins that an issue created with a
// priority renders the "• P<n>" chip in human list output.
func TestList_HumanRowRendersPriorityChip(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	type createResp struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	postJSON[createResp](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues",
		map[string]any{"actor": "tester", "title": "urgent fix", "priority": int64(1)})

	out := runCLI(t, env, dir, "list")

	assert.Contains(t, out, "• P1")
}

// TestList_HumanRowRendersBugLabelChip pins that a bug-labeled issue renders
// the "[bug] " chip in human list output.
func TestList_HumanRowRendersBugLabelChip(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "broken thing")
	runCLI(t, env, dir, "label", "add", ref, "bug")

	out := runCLI(t, env, dir, "list")

	assert.Contains(t, out, "[bug] ")
}

// TestList_HumanTreeIndentsChildrenUnderParent pins bd-style tree
// rendering: children move directly beneath their parent, non-last
// children connect with "├─ ", the last child with "└─ ", and the
// parent row stays unindented.
func TestList_HumanTreeIndentsChildrenUnderParent(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	parent := createIssue(t, env, pid, "parent epic")
	childA := createIssue(t, env, pid, "child alpha")
	childB := createIssue(t, env, pid, "child beta")
	runCLI(t, env, dir, "edit", childA, "--parent", parent)
	runCLI(t, env, dir, "edit", childB, "--parent", parent)

	out := runCLI(t, env, dir, "list")

	assert.Contains(t, out, "├─ ○ ", "non-last child connects with ├─")
	assert.Contains(t, out, "└─ ○ ", "last child connects with └─")
	parentIdx := strings.Index(out, "parent epic")
	require.GreaterOrEqual(t, parentIdx, 0)
	for _, title := range []string{"child alpha", "child beta"} {
		idx := strings.Index(out, title)
		require.GreaterOrEqual(t, idx, 0)
		assert.Greater(t, idx, parentIdx, "%s renders beneath its parent", title)
	}
}

// TestList_HumanTreeRendersGrandchildRecursively pins recursion through
// parent chains: a grandchild nests one level deeper with the rail
// continuation prefix.
func TestList_HumanTreeRendersGrandchildRecursively(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	root := createIssue(t, env, pid, "tree root")
	mid := createIssue(t, env, pid, "tree mid")
	leaf := createIssue(t, env, pid, "tree leaf")
	runCLI(t, env, dir, "edit", mid, "--parent", root)
	runCLI(t, env, dir, "edit", leaf, "--parent", mid)

	out := runCLI(t, env, dir, "list")

	assert.Contains(t, out, "└─ ○ ", "mid connects under root")
	assert.Contains(t, out, "   └─ ○ ", "leaf nests one level deeper")
}

// TestList_HumanTreeOrphanedChildRendersFlat pins the filter-mismatch
// fallback: when a child's parent does not match the active filter
// (here excluded via --no-label), the child renders flat at top level
// rather than being dropped or indented.
func TestList_HumanTreeOrphanedChildRendersFlat(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	parent := createIssue(t, env, pid, "filtered parent")
	child := createIssue(t, env, pid, "orphan child")
	runCLI(t, env, dir, "edit", child, "--parent", parent)
	runCLI(t, env, dir, "label", "add", parent, "hidden")

	out := runCLI(t, env, dir, "list", "--no-label", "hidden")

	assert.NotContains(t, out, "filtered parent")

	assert.Contains(t, out, "orphan child")
	assert.NotContains(t, out, "├─", "orphan renders flat, no connector")
	assert.NotContains(t, out, "└─", "orphan renders flat, no connector")
}

// TestList_AgentOutputUnchangedByTree pins that tree rendering is human
// mode only: agent rows keep the flat "- issue=..." shape with no
// box-drawing connectors.
func TestList_AgentOutputUnchangedByTree(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	parent := createIssue(t, env, pid, "agent parent")
	child := createIssue(t, env, pid, "agent child")
	runCLI(t, env, dir, "edit", child, "--parent", parent)

	out := runCLI(t, env, dir, "--agent", "list")

	assert.NotContains(t, out, "├─")
	assert.NotContains(t, out, "└─")
	assert.Contains(t, out, "- issue=")
}

// TestList_HumanRowBlockedGlyphFollowsOpenBlocker pins the Blocked
// derivation: an open issue with an OPEN blocker renders the blocked glyph
// "●", and once the blocker is closed (or the blocker starts out closed)
// the same issue renders the open glyph "○" instead.
func TestList_HumanRowBlockedGlyphFollowsOpenBlocker(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	blocker := createIssue(t, env, pid, "blocker issue")
	blocked := createIssue(t, env, pid, "blocked issue")
	createLinkViaHTTP(t, env, pid, blocker, "blocks", blocked)

	out := runCLI(t, env, dir, "list")
	blockedLine := findLineContaining(t, out, "blocked issue")
	assert.Contains(t, blockedLine, "●", "expected blocked glyph while blocker is open")

	runCLI(t, env, dir, "close", blocker, "--done", "--message",
		"resolved for test; blocker no longer applies", "--commit", "abc1234")

	out = runCLI(t, env, dir, "list", "--status", "all")
	blockedLine = findLineContaining(t, out, "blocked issue")
	assert.Contains(t, blockedLine, "○", "expected open glyph once blocker is closed")
	assert.NotContains(t, blockedLine, "●")
}

// TestList_HumanRowIgnoresBlockerInArchivedProject pins that `kata list`
// agrees with `kata ready`: an open blocker whose project has been archived
// must not mark the blocked issue as blocked. The issue lives in the bound
// workspace project; the blocker lives in a separate project that gets
// archived via the same path `kata projects remove` uses (RemoveProject).
func TestList_HumanRowIgnoresBlockerInArchivedProject(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ctx := t.Context()
	blockerProject, err := env.DB.CreateProject(ctx, "blocker-project")
	require.NoError(t, err)

	blocked := createIssue(t, env, pid, "blocked issue")
	blocker := createIssue(t, env, blockerProject.ID, "blocker issue")
	// Cross-project link target must be qualified ("project#short_id"); a
	// bare short_id resolves within the subject's own (blocker) project.
	createLinkViaHTTP(t, env, blockerProject.ID, blocker, "blocks", "kata#"+blocked)

	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: blockerProject.ID, Actor: "tester", Force: true,
	})
	require.NoError(t, err)

	out := runCLI(t, env, dir, "list")
	blockedLine := findLineContaining(t, out, "blocked issue")
	assert.Contains(t, blockedLine, "○",
		"blocker in an archived project must not render the blocked glyph")
	assert.NotContains(t, blockedLine, "●")
	assert.Contains(t, out, "Total: 1 issue (1 open)",
		"footer must count the issue as open, not blocked")
}

// TestList_HumanFooterShowsTotalsAndLegend pins the footer: non-quiet human
// list output includes the rule, a "Total: N issues" summary, and the
// "Status: ○ open" legend.
func TestList_HumanFooterShowsTotalsAndLegend(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "alpha")

	out := runCLI(t, env, dir, "list")

	assert.Contains(t, out, "Total: 1 issue")
	assert.Contains(t, out, "Status: ○ open")
}

// TestList_HumanFooterAbsentUnderQuiet pins that --quiet suppresses the
// footer entirely, even when rows were printed.
func TestList_HumanFooterAbsentUnderQuiet(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "alpha")

	out := runCLI(t, env, dir, "--quiet", "list")

	assert.NotContains(t, out, "Total:")
	assert.NotContains(t, out, "Status: ○ open")
}

// TestList_HumanFooterAbsentOnZeroRows pins that an empty result set prints
// no footer (no rule, no "Total:" line).
func TestList_HumanFooterAbsentOnZeroRows(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	out := runCLI(t, env, dir, "list")

	assert.Empty(t, out)
	assert.NotContains(t, out, "Total:")
}

// rowLines returns the leading non-blank lines of human list output — the
// issue rows themselves, stopping before the footer's blank separator line.
func rowLines(out string) []string {
	var lines []string
	for ln := range strings.SplitSeq(out, "\n") {
		if ln == "" {
			break
		}
		lines = append(lines, ln)
	}
	return lines
}

// findLineContaining returns the single line of out containing substr,
// failing the test if zero or more than one line matches.
func findLineContaining(t *testing.T, out, substr string) string {
	t.Helper()
	var matches []string
	for ln := range strings.SplitSeq(out, "\n") {
		if strings.Contains(ln, substr) {
			matches = append(matches, ln)
		}
	}
	require.Len(t, matches, 1, "expected exactly one line containing %q, got %v", substr, matches)
	return matches[0]
}

func TestList_AgentOutputRowsOmitAbsentOwner(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "unowned task")

	out := runCLI(t, env, dir, "--agent", "list")

	assert.Contains(t, out, "OK list count=1 complete=true\n")
	assert.Contains(t, out, `title="unowned task"`)
	assert.Contains(t, out, "revision=1")
	assert.NotContains(t, out, "owner=")
}

func TestList_AgentOutputEscapesQuotedTitle(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, `quoted "title"`)

	out := runCLI(t, env, dir, "--agent", "list")

	assert.Contains(t, out, "OK list count=1 complete=true\n")
	assert.Contains(t, out, "title="+strconv.Quote(`quoted "title"`))
}

// TestList_SanitizesAnsiAndNewlinesInTitle covers hammer-test
// finding #2: a malicious title containing ANSI escape sequences or
// embedded newlines must not reach stdout raw, where it could clear
// the screen, set the window title, or break row layout. Sanitized
// at the human-output boundary; the JSON path is exempt (agents need
// the raw bytes).
func TestList_SanitizesAnsiAndNewlinesInTitle(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "evil\x1b[2Jtitle\nwith newline")

	out := runCLI(t, env, dir, "list")
	assert.NotContains(t, out, "\x1b", "ESC reached stdout")
	// The newline in the title must be escaped (\n literal) so the
	// list row stays on one visual line. Only the row section (before
	// the footer's intentional blank separator lines) is checked.
	lines := strings.SplitSeq(strings.TrimRight(out, "\n"), "\n")
	for ln := range lines {
		if ln == "" {
			break // footer begins with a blank separator line
		}
		assert.NotEmpty(t, ln, "list output produced a blank row from injected newline")
	}
}

// TestList_HumanFooterShowsShowingWhenTruncated pins that when the returned
// page is exactly --limit rows, the human footer swaps "Total:" for
// "Showing:" so the summary doesn't misread as a project-wide total.
func TestList_HumanFooterShowsShowingWhenTruncated(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"alpha", "beta", "gamma"} {
		createIssue(t, env, pid, title)
	}

	out := runCLI(t, env, dir, "list", "--limit", "2")

	assert.Contains(t, out, "Showing: 2 issues")
	assert.NotContains(t, out, "Total:")
}

// TestList_HumanFooterShowsTotalWhenNotTruncated pins the non-truncated
// counterpart: when all matching rows fit under --limit, the footer keeps
// the "Total:" wording.
func TestList_HumanFooterShowsTotalWhenNotTruncated(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"alpha", "beta"} {
		createIssue(t, env, pid, title)
	}

	out := runCLI(t, env, dir, "list", "--limit", "10")

	assert.Contains(t, out, "Total: 2 issues")
	assert.NotContains(t, out, "Showing:")
}

// TestList_HintsWhenTruncated covers the silent-truncation pitfall: when the
// returned page is exactly --limit rows, the CLI prints a stderr hint so users
// realize there may be more. Hint goes to stderr so it doesn't pollute pipes
// (kata list | grep ...) and is suppressed in --json mode.
func TestList_HintsWhenTruncated(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"alpha", "beta", "gamma"} {
		createIssue(t, env, pid, title)
	}

	stdout, stderr, err := runCLIWithErr(t, env, dir, "list", "--limit", "2")
	require.NoError(t, err)
	// Two rows on stdout (before the footer's blank separator line), no
	// hint on stdout.
	rows := rowLines(strings.TrimRight(stdout, "\n"))
	assert.Len(t, rows, 2, "stdout should carry exactly --limit rows")
	assert.NotContains(t, stdout, "--limit", "hint must go to stderr, not stdout")
	// Hint on stderr.
	assert.Contains(t, stderr, "--limit",
		"stderr should hint that more rows may exist (stderr=%q)", stderr)
}

// TestList_NoHintWhenAllRowsFit guards the false-negative direction: when the
// page is shorter than --limit, no hint should fire.
func TestList_NoHintWhenAllRowsFit(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"alpha", "beta"} {
		createIssue(t, env, pid, title)
	}

	stdout, stderr, err := runCLIWithErr(t, env, dir, "list", "--limit", "10")
	require.NoError(t, err)
	assert.Contains(t, stdout, "alpha")
	assert.Contains(t, stdout, "beta")
	assert.NotContains(t, stderr, "--limit", "no hint expected when rows < limit")
}

// TestList_JSONOmitsHint pins that the JSON output path stays pure JSON. The
// hint is human-facing; agents consuming --json must not get extra stderr
// noise that breaks parsers expecting silent success.
func TestList_JSONOmitsHint(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"alpha", "beta", "gamma"} {
		createIssue(t, env, pid, title)
	}

	stdout, stderr, err := runCLIWithErr(t, env, dir, "--json", "list", "--limit", "2")
	require.NoError(t, err)
	assert.NotContains(t, stderr, "--limit", "JSON mode must suppress the hint")
	// stdout should still parse as JSON.
	var got struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Len(t, got.Issues, 2)
}

func TestList_EmptyStatusHints(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"first", "second", "third", "fourth"} {
		ref := createIssue(t, env, pid, title)
		runCLI(t, env, dir, "close", ref, "--done", "--message", "completed the work for this closed issue fixture", "--commit", "abc1234")
	}
	t.Chdir(dir)
	const message = "no open issues; 4 closed; see kata list --status all"
	for _, mode := range []string{"human", "--agent", "--json", "--quiet"} {
		args := []string{"list"}
		if mode != "human" {
			args = append(args, mode)
		}
		out, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), args...)
		require.NoError(t, err)
		switch mode {
		case "--json":
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			assert.Equal(t, []any{map[string]any{"code": "excluded_closed", "message": message + " --json", "next": "kata list --status all --json"}}, got["hints"])
			assert.Empty(t, stderr)
		case "--quiet":
			assert.Empty(t, out)
			assert.Empty(t, stderr)
		case "--agent":
			assert.Equal(t, "Hint: "+message+" --agent\n", stderr)
			assert.Equal(t, "OK list count=0 complete=true\n", out)
		default:
			assert.Equal(t, "Hint: "+message+"\n", stderr)
			assert.Empty(t, out)
		}
	}
	ref := createIssue(t, env, pid, "open match")
	runCLI(t, env, dir, "label", "add", ref, "bug")
	out, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--json", "--quiet", "list", "--status", "closed", "--label", "bug")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, []any{map[string]any{
		"code":    "excluded_open",
		"message": "no closed issues match the filters; 1 open match; see kata list --status all --json --quiet --label bug",
		"next":    "kata list --status all --json --quiet --label bug",
	}}, got["hints"])
	for _, args := range [][]string{{"list", "--status", "all"}, {"list"}, {"list", "--label", "missing"}} {
		_, stderr, err = executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), args...)
		require.NoError(t, err)
		assert.Empty(t, stderr)
	}
	out, _, err = executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--json", "--workspace", dir, "list", "--status", "closed", "--label", "bug")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "kata list --status all --workspace "+dir+" --json --label bug", got["hints"].([]any)[0].(map[string]any)["next"])
}

// Running the list retry from an unbound directory keeps the scope, output
// mode, limit, and membership filters. Without them, the newer unrelated
// issues would fill the one-row page and hide the counted issue. The quoted
// --no-label value excludes one nonexistent label; split, it would exclude the
// counted issue's "fix" label.
func TestList_EmptyStatusHintRetryKeepsFilters(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	hidden := createIssue(t, env, pid, "hidden match")
	runCLI(t, env, dir, "label", "add", hidden, "bug")
	runCLI(t, env, dir, "label", "add", hidden, "ui")
	runCLI(t, env, dir, "label", "add", hidden, "fix")
	runCLI(t, env, dir, "meta", "set", hidden, "work.attention", "needs review")
	runCLI(t, env, dir, "close", hidden, "--done", "--message", "completed the work for this closed issue fixture", "--commit", "abc1234")
	for _, title := range []string{"unrelated closed", "unrelated open"} {
		ref := createIssue(t, env, pid, title)
		runCLI(t, env, dir, "label", "add", ref, "bug")
		if title == "unrelated closed" {
			runCLI(t, env, dir, "close", ref, "--done", "--message", "completed the work for this closed issue fixture", "--commit", "abc1234")
		}
	}
	workspace := filepath.Join(t.TempDir(), "example workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	t.Chdir(t.TempDir())
	ctx := contextWithBaseURL(t.Context(), env.URL)

	out, stderr, err := executeRootCapture(t, ctx, "list", "--project", "kata", "--workspace", workspace, "--json", "--quiet", "--as", "example-agent",
		"--label", "bug", "--label", "ui", "--no-label", `"wont,fix"`, "--meta", "work.attention=needs review", "--limit", "1")
	require.NoError(t, err, stderr)
	assert.Empty(t, stderr)
	var got struct {
		Hints []cliHint `json:"hints"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Hints, 1)
	next := got.Hints[0].Next
	assert.Equal(t, "no open issues match the filters; 1 closed match; see "+next, got.Hints[0].Message)
	assert.NotContains(t, next, "--as")

	words, err := splitHelpWords(next)
	require.NoError(t, err)
	require.Equal(t, "kata", words[0])
	out, stderr, err = executeRootCapture(t, ctx, words[1:]...)
	require.NoError(t, err, stderr)
	var retry struct {
		Issues []struct {
			Title string `json:"title"`
		} `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &retry))
	require.Len(t, retry.Issues, 1)
	assert.Equal(t, "hidden match", retry.Issues[0].Title)
}

func TestList_EmptyStatusHintRequests(t *testing.T) {
	for _, scenario := range []string{"filtered", "global", "nonempty", "all-status", "cursor", "count-failed"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = fmt.Fprint(w, `{"ok":true,"api_schema_version":"0.27.0"}`)
				case "/api/v1/projects/resolve":
					_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"spoke-project"}}`)
				case "/api/v1/projects/1/issues", "/api/v1/issues":
					calls.Add(1)
					query := r.URL.Query()
					assert.Equal(t, []string{"bug", "urgent"}, query["label"])
					assert.Equal(t, "stale", query.Get("exclude_label"))
					assert.Equal(t, "alice", query.Get("owner"))
					assert.Equal(t, "2", query.Get("max_priority"))
					assert.Equal(t, "work.attention=stuck", query.Get("meta"))
					if query.Get("status") == "closed" {
						assert.Equal(t, "/api/v1/issues", r.URL.Path)
						if scenario == "global" {
							assert.Empty(t, query.Get("project_id"))
						} else {
							assert.Equal(t, "1", query.Get("project_id"))
						}
						assert.Equal(t, "1", query.Get("limit"))
						assert.Equal(t, "true", query.Get("include_total"))
						if scenario == "count-failed" {
							w.WriteHeader(http.StatusServiceUnavailable)
							_, _ = fmt.Fprint(w, `{"error":{"code":"unavailable","message":"count unavailable"}}`)
							return
						}
						_, _ = fmt.Fprint(w, `{"issues":[],"complete":true,"total":4}`)
					} else if scenario == "nonempty" {
						_, _ = fmt.Fprint(w, `{"issues":[{"short_id":"abcd","title":"a match","status":"open"}],"complete":true}`)
					} else {
						_, _ = fmt.Fprint(w, `{"issues":[],"complete":true}`)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			args := []string{"--json", "list", "--label", "bug", "--label", "urgent", "--no-label", "stale", "--owner", "alice", "--max-priority", "2", "--meta", "work.attention=stuck"}
			if scenario == "global" {
				args = append(args, "--all")
			} else {
				args = append(args, "--project", "spoke-project")
			}
			switch scenario {
			case "cursor":
				args = append(args, "--sort", "created", "--cursor", "next-page")
			case "all-status":
				args = append(args, "--status", "all")
			}
			out, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), args...)
			require.NoError(t, err)
			assert.Empty(t, stderr)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			if scenario == "filtered" || scenario == "global" {
				assert.EqualValues(t, 2, calls.Load())
				assert.Equal(t, "excluded_closed", got["hints"].([]any)[0].(map[string]any)["code"])
				const filters = " --max-priority 2 --owner alice --label bug --label urgent --no-label stale --meta work.attention=stuck"
				next := "kata list --status all --project spoke-project --json" + filters
				if scenario == "global" {
					next = "kata list --status all --json --all" + filters
				}
				assert.Equal(t, next, got["hints"].([]any)[0].(map[string]any)["next"])
			} else {
				assert.NotContains(t, got, "hints")
				wantCalls := int32(1)
				if scenario == "count-failed" {
					wantCalls = 2
				}
				assert.Equal(t, wantCalls, calls.Load())
			}
		})
	}
}
