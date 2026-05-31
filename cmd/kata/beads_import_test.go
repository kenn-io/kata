package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/testenv"
)

func TestImportBeadsRejectsInputAndTargetFlags(t *testing.T) {
	setupKataEnv(t)

	_, err := runCmdOutput(t, nil, "import", "--source-format", "beads", "--input", "beads.jsonl")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "--input")

	_, err = runCmdOutput(t, nil, "import", "--source-format", "beads", "--target", "target.db")
	ce = requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "--target")
}

func TestImportRejectsUnsupportedSourceFormat(t *testing.T) {
	setupKataEnv(t)

	_, err := runCmdOutput(t, nil, "import", "--source-format", "bogus")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "unsupported import format")
}

func TestImportLegacyFormatBeadsStillSelectsSource(t *testing.T) {
	setupKataEnv(t)

	_, err := runCmdOutput(t, nil, "import", "--format", "beads", "--input", "beads.jsonl")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "--input")
}

func TestImportBeadsMissingProjectUnattended(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	dir := t.TempDir()

	_, err := runCLICapture(t, env, dir, "import", "--source-format", "beads", "--json")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "run kata init first")
}

func TestImportBeadsAgentMissingProjectDoesNotPrompt(t *testing.T) {
	resetFlags(t)
	stubIsTTY(t, true)
	env := testenv.New(t)
	dir := t.TempDir()

	out, err := runBeadsImportTTY(t, env, dir, "y\n", "--agent")

	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "run kata init first")
	assert.NotContains(t, out, "Run kata init now?")
}

func TestImportBeadsFromLiveBD(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	installFakeBD(t)

	out, err := runCLICapture(t, env, dir, "import", "--source-format", "beads", "--as", "importer")
	require.NoError(t, err)
	assert.Contains(t, out, "imported beads: created 1, updated 0, unchanged 0, comments 1, links 0")

	// The importer created exactly one issue; look it up via the DB to get
	// its short_id, then show it.
	rows, err := env.DB.QueryContext(context.Background(),
		"SELECT short_id FROM issues WHERE project_id = ? LIMIT 1", pid)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next(), "imported issue not found")
	var short string
	require.NoError(t, rows.Scan(&short))

	show := runCLI(t, env, dir, "show", short)
	assert.Contains(t, show, "Imported from Beads")
	assert.Contains(t, show, "beads_id: b1")
	assert.Contains(t, show, "Comment body from beads")
}

func TestImportBeadsJSONSummaryFromLiveBD(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	installFakeBD(t)

	out, err := runCLICapture(t, env, dir, "import", "--source-format", "beads", "--json", "--as", "importer")
	require.NoError(t, err)

	var summary struct {
		Source    string   `json:"source"`
		Created   int      `json:"created"`
		Updated   int      `json:"updated"`
		Unchanged int      `json:"unchanged"`
		Comments  int      `json:"comments"`
		Links     int      `json:"links"`
		Errors    []string `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &summary), "json output: %s", out)
	assert.Equal(t, "beads", summary.Source)
	assert.Equal(t, 1, summary.Created)
	assert.Equal(t, 0, summary.Updated)
	assert.Equal(t, 0, summary.Unchanged)
	assert.Equal(t, 1, summary.Comments)
	assert.Equal(t, 0, summary.Links)
	assert.Empty(t, summary.Errors)
}

func TestImportBeadsAgentSummaryFromLiveBD(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	installFakeBD(t)

	out, err := runCLICapture(t, env, dir, "import", "--source-format", "beads", "--agent", "--as", "importer")
	require.NoError(t, err)

	assert.Equal(t, "OK import source_format=beads project="+itoa(pid)+" created=1 updated=0 unchanged=0 comments=1 links=0\n", out)
	assert.NotContains(t, out, "target=")
}

// TestBeadsImport_AssignsShortIDs pins that imported beads issues pick
// up a valid kata short_id from the auto-extend path — the importer
// creates fresh kata ULIDs and the short_id flows through unchanged in
// the per-item JSON output.
func TestBeadsImport_AssignsShortIDs(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	installFakeBD(t)

	out, err := runCLICapture(t, env, dir, "import", "--source-format", "beads", "--json", "--as", "importer")
	require.NoError(t, err)

	var result struct {
		Items []struct {
			ExternalID   string `json:"external_id"`
			IssueShortID string `json:"issue_short_id"`
			Status       string `json:"status"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result), "json output: %s", out)
	require.NotEmpty(t, result.Items, "import should report per-item results: %s", out)
	for _, item := range result.Items {
		assert.NotEmpty(t, item.ExternalID, "external_id must round-trip")
		assert.True(t, shortid.Valid(item.IssueShortID),
			"short_id %q for external_id %q must be valid", item.IssueShortID, item.ExternalID)
	}
}

func TestImportBeadsPromptsInitAndRetries(t *testing.T) {
	resetFlags(t)
	stubIsTTY(t, true)
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	installFakeBD(t)

	out, err := runBeadsImportTTY(t, env, dir, "y\n", "--as", "importer")
	require.NoError(t, err)
	assert.Contains(t, out, "No kata project found. Run kata init now? [y/N]")
	assert.Contains(t, out, "created and bound project")
	assert.Contains(t, out, "imported beads: created 1, updated 0, unchanged 0, comments 1, links 0")
	assert.FileExists(t, filepath.Join(dir, ".kata.toml"))
}

func TestImportBeadsPromptNoReturnsInitValidation(t *testing.T) {
	resetFlags(t)
	stubIsTTY(t, true)
	env := testenv.New(t)
	dir := t.TempDir()

	out, err := runBeadsImportTTY(t, env, dir, "n\n")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "run kata init first")
	assert.Contains(t, out, "No kata project found. Run kata init now? [y/N]")
}

func runBeadsImportTTY(t *testing.T, env *testenv.Env, dir, input string, args ...string) (string, error) {
	t.Helper()
	stdinPath := filepath.Join(t.TempDir(), "stdin")
	require.NoError(t, os.WriteFile(stdinPath, []byte(input), 0o600))
	stdin, err := os.Open(stdinPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	defer func() { _ = stdin.Close() }()

	stdoutPath := filepath.Join(t.TempDir(), "stdout")
	stdout, err := os.Create(stdoutPath) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	defer func() { _ = stdout.Close() }()

	cmd := newRootCmd()
	cmd.SetIn(stdin)
	cmd.SetOut(stdout)
	cmd.SetErr(stdout)
	cmd.SetArgs(append([]string{"--workspace", dir, "import", "--source-format", "beads"}, args...))
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))
	err = cmd.Execute()
	require.NoError(t, stdout.Sync())
	bs, readErr := os.ReadFile(stdoutPath) //nolint:gosec // test-controlled path
	require.NoError(t, readErr)
	return string(bs), err
}

func installFakeBD(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a POSIX shell script; beads-from-live-bd import isn't exercised on Windows")
	}
	bin := t.TempDir()
	path := filepath.Join(bin, "bd")
	script := `#!/bin/sh
set -eu
if [ "$1" = "export" ] && [ "$2" = "--no-memories" ]; then
cat <<'JSONL'
{"id":"b1","title":"Live bead","description":"Body from beads","status":"open","priority":1,"issue_type":"task","owner":"alice","created_at":"2026-05-01T10:00:00Z","created_by":"Alice","updated_at":"2026-05-01T10:00:00Z","labels":["Needs Review"],"comment_count":1}
JSONL
elif [ "$1" = "comments" ] && [ "$2" = "b1" ] && [ "$3" = "--json" ]; then
cat <<'JSON'
[{"id":"c1","issue_id":"b1","author":"Bob","text":"Comment body from beads","created_at":"2026-05-01T11:00:00Z"}]
JSON
else
	echo "unexpected bd args: $*" >&2
	exit 2
fi
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755)) //nolint:gosec // test fixture: fake bd script needs executable bit
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestParseBeadsExportAndBuildImportRequest(t *testing.T) {
	export := strings.NewReader(`{"id":"b1","title":"Blocker","description":"blocker body","status":"open","priority":1,"issue_type":"task","owner":"alice","created_at":"2026-05-01T10:00:00Z","created_by":"Alice","updated_at":"2026-05-01T10:00:00Z","labels":["Needs Review","bad label!","` + strings.Repeat("Very Long Label ", 8) + `"],"dependency_count":0,"dependent_count":1,"comment_count":0}
{"id":"b2","title":"Blocked","description":"blocked body","status":"closed","priority":2,"issue_type":"bug","owner":"bob","created_at":"2026-05-01T11:00:00Z","created_by":"Bob","updated_at":"2026-05-01T12:00:00Z","closed_at":"2026-05-01T12:00:00Z","close_reason":"fixed elsewhere","labels":[],"dependencies":[{"issue_id":"b2","depends_on_id":"b1","type":"blocks","created_at":"2026-05-01T11:30:00Z","created_by":"Bob","metadata":"{}"}],"comment_count":1}
`)
	commentJSON := strings.NewReader(`[{"id":"c1","issue_id":"b2","author":"Bob","text":"comment","created_at":"2026-05-01T11:45:00Z"}]`)
	parsedComments, err := parseBeadsCommentsJSON(commentJSON)
	require.NoError(t, err)
	comments := map[string][]beadsComment{"b2": parsedComments}

	req, err := buildBeadsImportRequest(export, comments, "importer")
	require.NoError(t, err)
	assert.Equal(t, "importer", req.Actor)
	assert.Equal(t, "beads", req.Source)
	require.Len(t, req.Items, 2)

	blocker := req.Items[0]
	assert.Equal(t, "b1", blocker.ExternalID)
	assert.Equal(t, "Blocker", blocker.Title)
	assert.Equal(t, "blocker body", strings.Split(blocker.Body, "\n---\n")[0])
	assert.Equal(t, "open", blocker.Status)
	assert.Equal(t, "Alice", blocker.Author)
	require.NotNil(t, blocker.Owner)
	assert.Equal(t, "alice", *blocker.Owner)
	assert.Equal(t, mustParseTime(t, "2026-05-01T10:00:00Z"), blocker.CreatedAt)
	assert.Equal(t, mustParseTime(t, "2026-05-01T10:00:00Z"), blocker.UpdatedAt)
	assert.Nil(t, blocker.ClosedAt)
	assert.Nil(t, blocker.ClosedReason)
	assert.Contains(t, blocker.Labels, "source:beads")
	assert.Contains(t, blocker.Labels, "beads-id:b1")
	assert.Contains(t, blocker.Labels, "needs-review")
	assert.Contains(t, blocker.Labels, "bad-label")
	for _, label := range blocker.Labels {
		assert.LessOrEqual(t, len(label), 64, "label %q should satisfy kata max length", label)
	}
	assert.Contains(t, blocker.Body, "Imported from Beads")
	assert.Contains(t, blocker.Body, "beads_id: b1")
	assert.Contains(t, blocker.Body, "beads_type: task")
	assert.NotContains(t, blocker.Body, "beads_priority:",
		"priority is now a first-class field; footer should not duplicate it")
	require.NotNil(t, blocker.Priority)
	assert.Equal(t, int64(1), *blocker.Priority)
	assert.Contains(t, blocker.Body, `beads_original_labels: ["Needs Review","bad label!"`)
	assert.NotContains(t, blocker.Body, "beads_dependencies")
	assert.NotContains(t, req.Items[1].Body, "beads_dependencies")
	assert.NotContains(t, req.Items[1].Body, "metadata")
	assert.NotContains(t, req.Items[1].Body, "created_by")
	assert.Contains(t, blocker.Body, "beads_created_at: 2026-05-01T10:00:00Z")
	assert.Contains(t, blocker.Body, "beads_updated_at: 2026-05-01T10:00:00Z")
	assert.Contains(t, blocker.Body, "beads_comment_count: 0")
	require.Len(t, blocker.Links, 1)
	assert.Equal(t, "blocks", blocker.Links[0].Type)
	assert.Equal(t, "b2", blocker.Links[0].TargetExternalID, "Beads A depends on B imports as B blocks A")

	blocked := req.Items[1]
	assert.Equal(t, "b2", blocked.ExternalID)
	assert.Equal(t, "closed", blocked.Status)
	require.NotNil(t, blocked.ClosedReason)
	assert.Equal(t, "done", *blocked.ClosedReason)
	require.NotNil(t, blocked.ClosedAt)
	assert.Equal(t, mustParseTime(t, "2026-05-01T12:00:00Z"), *blocked.ClosedAt)
	assert.Contains(t, blocked.Body, "beads_close_reason: fixed elsewhere")
	assert.Contains(t, blocked.Labels, "source:beads")
	assert.Contains(t, blocked.Labels, "beads-id:b2")
	require.Len(t, blocked.Comments, 1)
	assert.Equal(t, "c1", blocked.Comments[0].ExternalID)
	assert.Equal(t, "Bob", blocked.Comments[0].Author)
	assert.Equal(t, "comment", blocked.Comments[0].Body)
	assert.Equal(t, mustParseTime(t, "2026-05-01T11:45:00Z"), blocked.Comments[0].CreatedAt)
}

func TestNormalizeKataLabel(t *testing.T) {
	assert.Equal(t, "needs-review", normalizeKataLabel("Needs Review!"))
	assert.Equal(t, "imported", normalizeKataLabel("!!!"))
	assert.Equal(t, "source:beads", normalizeKataLabel("source:beads"))
	long := normalizeKataLabel(strings.Repeat("x", 100))
	assert.LessOrEqual(t, len(long), 64)
	assert.Regexp(t, `^[a-z0-9._:-]+$`, long)
}

func TestMapBeadsPriority(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want *int64
	}{
		{"zero is highest", 0, ptrInt64(0)},
		{"in range one", 1, ptrInt64(1)},
		{"top of range", 4, ptrInt64(4)},
		{"out of range above drops", 5, nil},
		{"negative drops", -1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapBeadsPriority(tc.in)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tc.want, *got)
		})
	}
}

func ptrInt64(n int64) *int64 { return &n }

func TestMapBeadsStatus(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", "open"},
		{"open", "open"},
		{"in_progress", "open"},
		{"blocked", "open"},
		{"ready", "open"},
		{"triage", "open"},
		{"todo", "open"},
		{"closed", "closed"},
		{"done", "closed"},
		{"merged", "closed"},
		{"resolved", "closed"},
		{"some-future-status", "open"}, // unknown defaults to open
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			assert.Equal(t, tc.want, mapBeadsStatus(tc.raw))
		})
	}
}

func TestBeadsImportsBlockedStatusAsOpenWithLabel(t *testing.T) {
	export := strings.NewReader(`{"id":"b1","title":"Active work","description":"body","status":"blocked","created_at":"2026-05-01T10:00:00Z","created_by":"Alice","updated_at":"2026-05-01T10:00:00Z"}`)
	req, err := buildBeadsImportRequest(export, nil, "importer")
	require.NoError(t, err)
	require.Len(t, req.Items, 1)
	assert.Equal(t, "open", req.Items[0].Status,
		"beads 'blocked' must collapse to kata 'open' so the import doesn't bail")
	assert.Contains(t, req.Items[0].Labels, "beads-status:blocked",
		"original beads status should be preserved as a label")
}

func TestBeadsImportsMergedStatusAsClosed(t *testing.T) {
	export := strings.NewReader(`{"id":"b1","title":"Shipped","description":"body","status":"merged","close_reason":"shipped","closed_at":"2026-05-02T10:00:00Z","created_at":"2026-05-01T10:00:00Z","created_by":"Alice","updated_at":"2026-05-02T10:00:00Z"}`)
	req, err := buildBeadsImportRequest(export, nil, "importer")
	require.NoError(t, err)
	require.Len(t, req.Items, 1)
	assert.Equal(t, "closed", req.Items[0].Status)
	require.NotNil(t, req.Items[0].ClosedReason)
	assert.Equal(t, "done", *req.Items[0].ClosedReason,
		"non-canonical close_reason 'shipped' falls back to kata's 'done'")
}

func TestBeadsRejectsDependencyTargetMissingFromExport(t *testing.T) {
	export := strings.NewReader(`{"id":"b2","title":"Blocked","description":"body","status":"open","created_at":"2026-05-01T10:00:00Z","created_by":"Alice","updated_at":"2026-05-01T10:00:00Z","dependencies":[{"issue_id":"b2","depends_on_id":"missing","type":"blocks"}]}`)
	_, err := buildBeadsImportRequest(export, nil, "importer")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dependency target")
}

func TestBeadsRejectsOversizedCommentsJSON(t *testing.T) {
	_, err := parseBeadsCommentsJSON(io.LimitReader(repeatedByteReader(' '), maxBeadsCommentsJSONBytes+1))
	require.Error(t, err)
	var ce *cliError
	require.True(t, errors.As(err, &ce), "expected validation cliError, got %T", err)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Equal(t, ExitValidation, ce.ExitCode)
	assert.Contains(t, err.Error(), "beads comments JSON exceeds")
}

type repeatedByteReader byte

func (r repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return ts
}
