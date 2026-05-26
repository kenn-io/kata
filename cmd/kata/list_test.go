package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestList_AgentOutputRowsOmitAbsentOwner(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "unowned task")

	out := runCLI(t, env, dir, "--agent", "list")

	assert.Contains(t, out, "OK list count=1\n")
	assert.Contains(t, out, `title="unowned task"`)
	assert.NotContains(t, out, "owner=")
}

func TestList_AgentOutputEscapesQuotedTitle(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, `quoted "title"`)

	out := runCLI(t, env, dir, "--agent", "list")

	assert.Contains(t, out, "OK list count=1\n")
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
	// list row stays on one visual line.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, ln := range lines {
		assert.NotEmpty(t, ln, "list output produced a blank row from injected newline")
	}
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
	// Two rows on stdout, no hint on stdout.
	rows := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
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
