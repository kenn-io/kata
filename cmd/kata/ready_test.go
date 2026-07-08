package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReady_OutputsShortIDNotNumber pins the JSON wire shape: each ready
// row carries short_id; the legacy `number` field is gone.
func TestReady_OutputsShortIDNotNumber(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "first")

	out, err := runCmdOutput(t, env, "--workspace", dir, "--json", "ready")
	require.NoError(t, err)
	var got struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotEmpty(t, got.Issues)
	first := got.Issues[0]
	_, hasShort := first["short_id"]
	_, hasNumber := first["number"]
	assert.True(t, hasShort, "short_id missing from ready row: %v", first)
	assert.False(t, hasNumber, "number still present in ready row: %v", first)
}

func TestReady_FiltersBlocked(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	blocker := createIssue(t, env, pid, "blocker")
	blocked := createIssue(t, env, pid, "blocked")
	createIssue(t, env, pid, "standalone")
	createLinkViaHTTP(t, env, pid, blocker, "blocks", blocked)

	out := runCLI(t, env, dir, "ready")
	assert.Contains(t, out, "blocker")
	assert.Contains(t, out, "standalone")
	assert.False(t, strings.Contains(out, "blocked"),
		"blocked is hidden while blocker is open")
}

func TestReady_AgentOutputRowsOmitAbsentOwner(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "ready unowned")

	out := runCLI(t, env, dir, "--agent", "ready")

	assert.Contains(t, out, "OK ready count=1\n")
	assert.Contains(t, out, `title="ready unowned"`)
	assert.NotContains(t, out, "owner=")
}

// TestReady_AgentAllEmitsKVRows pins the agent-mode contract for the global
// path: `--agent ready --all` emits the structured `OK ready count=N` header
// plus one kv row per issue whose issue field is the qualified
// "<project>#<short_id>" ref — never the human glyph rows.
func TestReady_AgentAllEmitsKVRows(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	sid := createIssue(t, env, pid, "agent all row")

	out, err := runCmdOutput(t, env, "--workspace", dir, "--agent", "ready", "--all")
	require.NoError(t, err)

	assert.Contains(t, out, "OK ready count=1\n")
	assert.Contains(t, out, "- issue=kata#"+sid)
	assert.Contains(t, out, `title="agent all row"`)
	assert.NotContains(t, out, "○ ", "agent mode must not emit human glyph rows")
	assert.NotContains(t, out, "Ready:", "agent mode must not emit the human footer")
}

// TestReady_AgentAllRowCarriesPriorityAndOmitsAbsentOwner pins the field set
// on --all agent rows: priority is rendered when set, owner is omitted when
// absent (same optional-field idiom as the project-scoped agent path).
func TestReady_AgentAllRowCarriesPriorityAndOmitsAbsentOwner(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	postJSON[map[string]any](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues",
		map[string]any{"actor": "tester", "title": "urgent fix", "priority": int64(1)})

	out, err := runCmdOutput(t, env, "--workspace", dir, "--agent", "ready", "--all")
	require.NoError(t, err)

	assert.Contains(t, out, "priority=1")
	assert.NotContains(t, out, "owner=")
}

func TestReady_UnownedAndOwnerMutualExclusion(t *testing.T) {
	env, dir := setupCLIEnv(t)
	resetFlags(t)
	_, err := runCLICapture(t, env, dir, "ready", "--unowned", "--owner", "alice")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestReady_AllFlagListsAcrossProjects(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "in-bound-project")

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all")
	require.NoError(t, err)
	// Text rows must use the qualified short-ref form: "<project>#<short_id>".
	// We don't pin the project name (depends on setupCLIWorkspace), but the
	// "#" separator is the contract.
	assert.Contains(t, out, "#",
		"--all output uses qualified refs (project#short_id), got: %q", out)
}

func TestReady_AllFlagJSONIncludesProjectName(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "first")

	out, err := runCmdOutput(t, env, "--workspace", dir, "--json", "ready", "--all")
	require.NoError(t, err)
	var got struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotEmpty(t, got.Issues)
	first := got.Issues[0]
	_, hasProject := first["project_name"]
	assert.True(t, hasProject, "project_name missing from --all JSON row: %v", first)
}

func TestReady_AllAndProjectAreMutuallyExclusive(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	_, err := runCmdOutput(t, env, "--workspace", dir,
		"--project", "anything", "ready", "--all")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestReady_AllFromBoundDirSkipsLocalProject pins that --all does not require
// (or use) the local .kata.toml project context: an agent in a bound workspace
// can still get the global view.
func TestReady_AllFromBoundDirSkipsLocalProject(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "from-bound-project")

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, "#",
		"--all from bound dir still emits qualified refs, got: %q", out)
}

// TestReady_AllRejectsFilterFlags pins that --all errors out when combined
// with per-project filter flags rather than silently dropping them: the
// global ready endpoint does not apply --unowned / --owner / --label /
// --no-label, so accepting those alongside --all would return misleading
// (unfiltered) results.
// TestReady_HumanRowUsesGlyphLayout pins that the project-scoped human
// path renders through the shared row renderer: open glyph + title.
func TestReady_HumanRowUsesGlyphLayout(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "alpha")

	out := runCLI(t, env, dir, "ready")

	assert.Contains(t, out, "○ ", "expected open glyph prefix in human ready output")
	assert.Contains(t, out, "alpha")
}

// TestReady_HumanRowRendersPriorityChip pins that a ready issue with a
// priority renders the "• P<n>" chip in project-scoped human output.
func TestReady_HumanRowRendersPriorityChip(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	postJSON[map[string]any](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues",
		map[string]any{"actor": "tester", "title": "urgent fix", "priority": int64(1)})

	out := runCLI(t, env, dir, "ready")

	assert.Contains(t, out, "• P1")
}

// TestReady_HumanFooterShowsSummaryAndLegend pins the ready footer: rule,
// "Ready: N issues with no active blockers" summary, and the
// "Status: ○ open" legend (no "● blocked" clause since ready results are
// by definition unblocked).
func TestReady_HumanFooterShowsSummaryAndLegend(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "alpha")

	out := runCLI(t, env, dir, "ready")

	assert.Contains(t, out, "Ready: 1 issue with no active blockers")
	assert.Contains(t, out, "Status: ○ open")
	assert.NotContains(t, out, "● blocked")
}

// TestReady_HumanFooterAbsentUnderQuiet pins that --quiet suppresses the
// ready footer even when rows were printed.
func TestReady_HumanFooterAbsentUnderQuiet(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "alpha")

	out := runCLI(t, env, dir, "--quiet", "ready")

	assert.NotContains(t, out, "Ready:")
	assert.NotContains(t, out, "Status: ○ open")
}

// TestReady_HumanFooterAbsentOnZeroRows pins that an empty ready result
// prints no footer at all.
func TestReady_HumanFooterAbsentOnZeroRows(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	out := runCLI(t, env, dir, "ready")

	assert.NotContains(t, out, "Ready:")
}

// TestReady_HumanFooterShowsShowingWhenTruncated pins that when the
// returned page is exactly --limit rows, the scoped ready footer swaps
// "Ready:" for "Showing:" so the summary doesn't misread as a full count
// of ready issues.
func TestReady_HumanFooterShowsShowingWhenTruncated(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"alpha", "beta", "gamma"} {
		createIssue(t, env, pid, title)
	}

	out := runCLI(t, env, dir, "ready", "--limit", "2")

	assert.Contains(t, out, "Showing: 2 ready issues with no active blockers")
	assert.NotContains(t, out, "Ready:")
}

// TestReady_HumanFooterShowsReadyWhenNotTruncated pins the non-truncated
// counterpart: when all ready rows fit under --limit, the footer keeps
// the "Ready:" wording.
func TestReady_HumanFooterShowsReadyWhenNotTruncated(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"alpha", "beta"} {
		createIssue(t, env, pid, title)
	}

	out := runCLI(t, env, dir, "ready", "--limit", "10")

	assert.Contains(t, out, "Ready: 2 issues with no active blockers")
	assert.NotContains(t, out, "Showing:")
}

// TestReady_AllHumanFooterShowsShowingWhenTruncated pins the --all path's
// truncation wording, mirroring the scoped-project case above.
func TestReady_AllHumanFooterShowsShowingWhenTruncated(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	for _, title := range []string{"alpha", "beta", "gamma"} {
		createIssue(t, env, pid, title)
	}

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all", "--limit", "2")
	require.NoError(t, err)

	assert.Contains(t, out, "Showing: 2 ready issues with no active blockers")
	assert.NotContains(t, out, "Ready:")
}

// TestReady_AllHumanRowUsesGlyphLayoutAndQualifiedID pins that the --all
// human path renders through the shared row renderer with the qualified
// "project#short_id" id and the open glyph.
func TestReady_AllHumanRowUsesGlyphLayoutAndQualifiedID(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "alpha")

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all")
	require.NoError(t, err)

	assert.Contains(t, out, "○ ", "expected open glyph prefix in --all human ready output")
	assert.Contains(t, out, "#")
	assert.Contains(t, out, "alpha")
}

// TestReady_AllHumanRowRendersPriorityChip pins that --all human output
// renders the priority chip for a ready issue created with a priority.
func TestReady_AllHumanRowRendersPriorityChip(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	postJSON[map[string]any](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues",
		map[string]any{"actor": "tester", "title": "urgent fix", "priority": int64(1)})

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all")
	require.NoError(t, err)

	assert.Contains(t, out, "• P1")
}

// TestReady_AllHumanFooterShowsSummaryAndLegend pins the --all footer.
func TestReady_AllHumanFooterShowsSummaryAndLegend(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "alpha")

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all")
	require.NoError(t, err)

	assert.Contains(t, out, "Ready: 1 issue with no active blockers")
	assert.Contains(t, out, "Status: ○ open")
}

func TestReady_AllRejectsFilterFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unowned", []string{"ready", "--all", "--unowned"}},
		{"owner", []string{"ready", "--all", "--owner", "alice"}},
		{"label", []string{"ready", "--all", "--label", "bug"}},
		{"no-label", []string{"ready", "--all", "--no-label", "wip"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, dir, _ := setupCLIWorkspace(t)
			args := append([]string{"--workspace", dir}, tc.args...)
			_, err := runCmdOutput(t, env, args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--all does not support")
		})
	}
}
