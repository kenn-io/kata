package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDigest_HumanRender(t *testing.T) {
	f := newCLIFixture(t)
	first := createIssueViaHTTP(t, f.env, f.dir, "first")
	second := createIssueViaHTTP(t, f.env, f.dir, "second")

	require.NoError(t, f.execute("digest", "--since", "1h"))
	out := f.buf.String()
	assert.Contains(t, out, "digest ")
	assert.Contains(t, out, "created=2")
	// Each issue should appear by its short_id (not a numeric ref).
	assert.Contains(t, out, first)
	assert.Contains(t, out, second)
	assert.Contains(t, out, "created")
}

func TestDigest_AgentOutputShape(t *testing.T) {
	env, dir := setupCLIEnv(t)
	short := createIssueViaHTTP(t, env, dir, "first")

	out := runCLI(t, env, dir, "--agent", "digest", "--since", "1h")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "agent digest should include header and rows: %q", out)
	assert.Regexp(t, `^OK digest count=\d+`, lines[0])
	assert.Contains(t, lines[1], "actor=tester")
	assert.Contains(t, lines[1], "issue="+short)
	assert.Contains(t, lines[1], "actions=created")
	assert.NotContains(t, out, "\x1b", "agent output must not contain ANSI escape bytes")
}

// TestDigest_OutputShape pins the JSON wire shape: per-issue rows carry
// issue_short_id and issue_uid; the legacy issue_number field is gone.
func TestDigest_OutputShape(t *testing.T) {
	f := newCLIFixture(t)
	createIssueViaHTTP(t, f.env, f.dir, "alpha")

	require.NoError(t, f.execute("--json", "digest", "--since", "1h"))
	var got struct {
		Actors []struct {
			Issues []map[string]any `json:"issues"`
		} `json:"actors"`
	}
	require.NoError(t, json.Unmarshal(f.buf.Bytes(), &got))
	require.NotEmpty(t, got.Actors)
	require.NotEmpty(t, got.Actors[0].Issues)
	row := got.Actors[0].Issues[0]
	_, hasShort := row["issue_short_id"]
	_, hasUID := row["issue_uid"]
	_, hasNumber := row["issue_number"]
	assert.True(t, hasShort, "issue_short_id missing from digest row: %v", row)
	assert.True(t, hasUID, "issue_uid missing from digest row: %v", row)
	assert.False(t, hasNumber, "issue_number still present in digest row: %v", row)
}

func TestDigest_RejectsBadSince(t *testing.T) {
	f := newCLIFixture(t)

	err := f.execute("digest", "--since", "blarg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid time spec")
}

func TestDigest_ParseSinceUntil(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	got, err := parseSinceUntil("24h", now)
	require.NoError(t, err)
	assert.Equal(t, now.Add(-24*time.Hour), got)

	got, err = parseSinceUntil("7d", now)
	require.NoError(t, err)
	assert.Equal(t, now.Add(-7*24*time.Hour), got)

	got, err = parseSinceUntil("2026-05-01T00:00:00Z", now)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), got)

	_, err = parseSinceUntil("garbage", now)
	require.Error(t, err)
}
