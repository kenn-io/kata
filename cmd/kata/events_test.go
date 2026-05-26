package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestEvents_OneShotPlainOutput(t *testing.T) {
	f := newCLIFixture(t)
	createIssueViaHTTP(t, f.env, f.dir, "first")
	createIssueViaHTTP(t, f.env, f.dir, "second")

	require.NoError(t, f.execute("events"))

	out := f.buf.String()
	assert.Contains(t, out, "issue.created")
	lines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	assert.Equal(t, 2, lines)
}

func TestEvents_OneShotJSON(t *testing.T) {
	f := newCLIFixture(t)
	createIssueViaHTTP(t, f.env, f.dir, "only")

	require.NoError(t, f.execute("events", "--json"))

	var b struct {
		KataAPIVersion int `json:"kata_api_version"`
		Events         []struct {
			EventID    int64   `json:"event_id"`
			Type       string  `json:"type"`
			ProjectUID string  `json:"project_uid"`
			IssueUID   *string `json:"issue_uid"`
		} `json:"events"`
		NextAfterID int64 `json:"next_after_id"`
	}
	require.NoError(t, json.Unmarshal(f.buf.Bytes(), &b))
	assert.Equal(t, 1, b.KataAPIVersion)
	require.Len(t, b.Events, 1)
	assert.Equal(t, "issue.created", b.Events[0].Type)
	assert.NotEmpty(t, b.Events[0].ProjectUID)
	require.NotNil(t, b.Events[0].IssueUID)
	assert.NotEmpty(t, *b.Events[0].IssueUID)
	assert.Equal(t, int64(1), b.NextAfterID)
}

func TestEvents_OneShotAgentOutput(t *testing.T) {
	env, dir := setupCLIEnv(t)
	first := runCLI(t, env, dir, "--quiet", "--as", "wesm", "create", "first")
	second := runCLI(t, env, dir, "--quiet", "--as", "wesm", "create", "second")

	out := runCLI(t, env, dir, "--agent", "events")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3, "agent output should be header plus one row per event: %q", out)
	assert.Equal(t, "OK events count=2 next_after_id=2", lines[0])
	assert.Equal(t, "- id=1 type=issue.created project=kata issue="+first+" actor=wesm", lines[1])
	assert.Equal(t, "- id=2 type=issue.created project=kata issue="+second+" actor=wesm", lines[2])
}

func TestEvents_OneShotAgentResetRequired(t *testing.T) {
	env, dir := setupCLIEnv(t)
	short := createIssueViaHTTP(t, env, dir, "doomed")

	runCLI(t, env, dir, "purge", short, "--force", "--confirm", "PURGE kata#"+short)

	out := runCLI(t, env, dir, "--agent", "events", "--after", "0")

	assert.Regexp(t, `^OK events reset_required=true reset_after_id=\d+\n?$`, out)
}

func TestEvents_OneShotAllProjectsHitsCrossProject(t *testing.T) {
	env := testenv.New(t)
	dirA := initBoundWorkspace(t, env.URL, "https://github.com/wesm/a.git")
	dirB := initBoundWorkspace(t, env.URL, "https://github.com/wesm/b.git")
	createIssueViaHTTP(t, env, dirA, "a-issue")
	createIssueViaHTTP(t, env, dirB, "b-issue")

	out := requireCmdOutput(t, env, "events", "--all-projects", "--json")

	var b struct {
		Events []struct {
			ProjectID int64 `json:"project_id"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &b))
	assert.Len(t, b.Events, 2, "all-projects must include both projects")
}

func TestEvents_OneShotAllProjectsAgentIncludesProject(t *testing.T) {
	env := testenv.New(t)
	dirA := initBoundWorkspace(t, env.URL, "https://github.com/wesm/a.git")
	dirB := initBoundWorkspace(t, env.URL, "https://github.com/wesm/b.git")
	createIssueViaHTTP(t, env, dirA, "a-issue")
	createIssueViaHTTP(t, env, dirB, "b-issue")

	out := requireCmdOutput(t, env, "events", "--all-projects", "--agent")

	assert.Contains(t, out, "project=a")
	assert.Contains(t, out, "project=b")
}

func TestEvents_TailAgentEmitsOneLinePerEvent(t *testing.T) {
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://github.com/wesm/kata.git")

	ctx, cancel := context.WithTimeout(contextWithBaseURL(context.Background(), env.URL), 5*time.Second)
	defer cancel()
	a := startAsyncCLI(t, ctx, "--workspace", dir, "--agent", "events", "--tail")
	defer a.stop()

	time.Sleep(200 * time.Millisecond)
	first := createIssueViaHTTP(t, env, dir, "first")
	second := createIssueViaHTTP(t, env, dir, "second")

	out := a.awaitOutput(func(s string) bool {
		return strings.Count(s, "OK event ") >= 2
	}, 2*time.Second)

	lines := []string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	require.GreaterOrEqual(t, len(lines), 2, "expected at least 2 agent event lines, got: %q", out)
	for _, l := range lines[:2] {
		assert.Regexp(t, `^OK event id=\d+ type=issue\.created issue=\S+ project=kata actor=tester$`, l)
		assert.NotContains(t, l, "{", "agent tail output must not be NDJSON")
	}
	assert.Contains(t, lines[0], "issue="+first)
	assert.Contains(t, lines[1], "issue="+second)
}

func TestEvents_TailAgentProgressUsesSSEID(t *testing.T) {
	input := strings.Join([]string{
		"id: 42",
		"event: issue.created",
		`data: {"event_id":7,"type":"issue.created","issue_short_id":"abc4","actor":"wesm"}`,
		"",
		"",
	}, "\n")
	var out bytes.Buffer

	res, err := parseSSEStream(bufio.NewReader(strings.NewReader(input)), 0, &out, outputAgent)

	require.NoError(t, err)
	assert.Equal(t, "OK event id=7 type=issue.created issue=abc4 actor=wesm\n", out.String())
	assert.Equal(t, int64(42), res.Progress.lastID)
}

func TestEvents_TailAgentResetRequired(t *testing.T) {
	input := strings.Join([]string{
		"id: 100",
		"event: sync.reset_required",
		`data: {"reset_after_id":100}`,
		"",
		"",
	}, "\n")
	var out bytes.Buffer

	res, err := parseSSEStream(bufio.NewReader(strings.NewReader(input)), 0, &out, outputAgent)

	require.NoError(t, err)
	require.NotNil(t, res.Reset)
	assert.Equal(t, int64(100), res.Reset.newCursor)
	assert.Equal(t, "OK events reset_required=true reset_after_id=100\n", out.String())
}

func TestEvents_TailEmitsNDJSON(t *testing.T) {
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://github.com/wesm/kata.git")

	ctx, cancel := context.WithTimeout(contextWithBaseURL(context.Background(), env.URL), 5*time.Second)
	defer cancel()
	a := startAsyncCLI(t, ctx, "--workspace", dir, "events", "--tail", "--json")
	defer a.stop()

	time.Sleep(200 * time.Millisecond)
	createIssueViaHTTP(t, env, dir, "first")
	createIssueViaHTTP(t, env, dir, "second")

	out := a.awaitOutput(func(s string) bool {
		return strings.Count(s, "issue.created") >= 2
	}, 2*time.Second)

	lines := []string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	require.GreaterOrEqual(t, len(lines), 2, "expected at least 2 NDJSON lines, got: %q", out)
	for _, l := range lines[:2] {
		var envObj map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &envObj), "each line must be a JSON object")
		assert.Equal(t, "issue.created", envObj["type"])
		assert.NotEmpty(t, envObj["project_uid"])
		assert.NotEmpty(t, envObj["issue_uid"])
	}
}

func TestEvents_NegativeAfterRejected(t *testing.T) {
	_, err := runCmdOutput(t, nil, "events", "--all-projects", "--after=-1")
	ce := requireCLIError(t, err, ExitUsage)
	assert.Contains(t, ce.Message, "non-negative")
}

func TestEvents_NegativeLastEventIDRejected(t *testing.T) {
	_, err := runCmdOutput(t, nil, "events", "--all-projects", "--tail", "--last-event-id=-1")
	ce := requireCLIError(t, err, ExitUsage)
	assert.Contains(t, ce.Message, "non-negative")
}

// TestEvents_TailFailsFastOn4xx pins the spec §7.2 rule: HTTP 4xx responses
// are terminal, not retryable. A bad cursor or unknown project must surface
// to the caller, not spin in the reconnect loop.
func TestEvents_TailFailsFastOn4xx(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	ctx, cancel := context.WithTimeout(contextWithBaseURL(context.Background(), env.URL), 5*time.Second)
	defer cancel()
	_, _, err := executeRootCapture(t, ctx, "events", "--project-id", "99999", "--tail")
	require.Error(t, err, "tail must surface 404 instead of looping")
	assert.Contains(t, err.Error(), "404")
}

func TestEvents_TailFollowsResetRequired(t *testing.T) {
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://github.com/wesm/kata.git")
	short := createIssueViaHTTP(t, env, dir, "doomed")

	ctx, cancel := context.WithTimeout(contextWithBaseURL(context.Background(), env.URL), 5*time.Second)
	defer cancel()
	// --json so the reset envelope is emitted as JSON for the assertion;
	// text mode would surface a reset line in a different shape.
	a := startAsyncCLI(t, ctx, "--workspace", dir, "events", "--tail", "--json")
	defer a.stop()

	time.Sleep(300 * time.Millisecond)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	project, err := env.DB.ProjectByID(t.Context(), pid)
	require.NoError(t, err)
	purgeURL := env.URL + "/api/v1/projects/" + itoa(pid) + "/issues/" + short + "/actions/purge"
	body := strings.NewReader(`{"actor":"tester"}`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, purgeURL, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kata-Confirm", "PURGE "+project.Name+"#"+short)
	resp, err := env.HTTP.Do(req) //nolint:gosec // G704: test server URL, not user-controlled
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	out := a.awaitOutput(func(s string) bool {
		return strings.Contains(s, `"reset_required":true`)
	}, 2*time.Second)
	assert.Contains(t, out, `"reset_required":true`,
		"--tail must emit a reset envelope when the daemon sends sync.reset_required")
}

// TestEvents_TailRejectsOneShotFlags covers hammer-test finding #6:
// --tail with --limit or --after used to be silently accepted, even
// though those flags are documented as one-shot mode. --limit 1
// still streamed indefinitely. Now both reject as kindUsage.
func TestEvents_TailRejectsOneShotFlags(t *testing.T) {
	for _, args := range [][]string{
		{"events", "--tail", "--limit", "1"},
		{"events", "--tail", "--after", "5"},
	} {
		_, err := runCmdOutput(t, nil, args...)
		ce := requireCLIError(t, err, ExitUsage)
		assert.Equalf(t, kindUsage, ce.Kind, "args %v: wrong kind", args)
	}
}

// TestEvents_OneShotRejectsTailFlag mirrors the symmetric case:
// --last-event-id is documented as --tail-only, so passing it without
// --tail should reject loudly instead of being silently ignored.
func TestEvents_OneShotRejectsTailFlag(t *testing.T) {
	_, err := runCmdOutput(t, nil, "events", "--last-event-id", "5")
	_ = requireCLIError(t, err, ExitUsage)
}

// TestEvents_OneShotRejectsNonPositiveLimit: parallel to list/ready,
// --limit 0/-1 in one-shot mode rejects with kindValidation. Search
// has the same check after hammer-test #5.
func TestEvents_OneShotRejectsNonPositiveLimit(t *testing.T) {
	for _, lim := range []string{"0", "-1"} {
		_, err := runCmdOutput(t, nil, "events", "--limit", lim)
		_ = requireCLIError(t, err, ExitValidation)
	}
}

// TestEvents_PayloadShape pins the JSON wire shape: each event row carries
// issue_short_id (display) and issue_uid (canonical); the legacy
// issue_number field is gone.
func TestEvents_PayloadShape(t *testing.T) {
	f := newCLIFixture(t)
	createIssueViaHTTP(t, f.env, f.dir, "first")

	require.NoError(t, f.execute("--json", "events"))
	var got struct {
		Events []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal(f.buf.Bytes(), &got))
	require.NotEmpty(t, got.Events)
	ev := got.Events[0]
	_, hasShort := ev["issue_short_id"]
	_, hasUID := ev["issue_uid"]
	_, hasNumber := ev["issue_number"]
	assert.True(t, hasShort, "issue_short_id missing from event row: %v", ev)
	assert.True(t, hasUID, "issue_uid missing from event row: %v", ev)
	assert.False(t, hasNumber, "issue_number still present in event row: %v", ev)
}
