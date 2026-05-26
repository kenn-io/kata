package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	assert.Equal(t, "OK search count=0 query=\"login race\"\n", out)
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
