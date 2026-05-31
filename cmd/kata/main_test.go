package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestRoot_HelpListsUniversalFlags(t *testing.T) {
	out := string(executeRoot(t, newRootCmd(), "--help"))
	assert.Contains(t, out, "--format")
	assert.Contains(t, out, "--json")
	assert.Contains(t, out, "--agent")
	assert.Contains(t, out, "--quiet")
	assert.Contains(t, out, "--as")
	assert.Contains(t, out, "--workspace")
	assertNoFederationStorageInternals(t, out)
}

func TestRootHelpMentionsFederationWithoutStorageInternals(t *testing.T) {
	out := string(executeRoot(t, newRootCmd(), "--help"))
	assert.Contains(t, strings.ToLower(out), "federation")
	assertNoFederationStorageInternals(t, out)
}

func TestDaemonHelpDoesNotMentionFederation(t *testing.T) {
	out := string(executeRoot(t, newRootCmd(), "daemon", "--help"))
	assertNoFederationInternals(t, out)
}

func TestImportHelpDoesNotMentionFederationInternals(t *testing.T) {
	out := string(executeRoot(t, newRootCmd(), "import", "--help"))
	assertNoFederationInternals(t, out)
}

func TestNormalCommandsDoNotMentionFederation(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, "ordinary work")

	for name, out := range map[string]string{
		"create":        runCLI(t, env, dir, "create", "plain issue"),
		"list":          runCLI(t, env, dir, "list"),
		"show":          runCLI(t, env, dir, "show", short),
		"projects list": requireCmdOutput(t, env, "projects", "list"),
		"projects show": requireCmdOutput(t, env, "projects", "show", itoa(pid)),
	} {
		assertNoFederationInternals(t, out, name)
	}
}

func assertNoFederationInternals(t *testing.T, out string, msgAndArgs ...any) {
	t.Helper()
	lower := strings.ToLower(out)
	for _, forbidden := range []string{
		"federation",
		"enrollment",
		"enrollments",
		"hlc",
		"origin_instance_uid",
		"push",
		"pull",
		"hidden setup",
	} {
		assert.NotContains(t, lower, forbidden, msgAndArgs...)
	}
}

func assertNoFederationStorageInternals(t *testing.T, out string, msgAndArgs ...any) {
	t.Helper()
	lower := strings.ToLower(out)
	for _, forbidden := range []string{
		"enrollment",
		"enrollments",
		"hlc",
		"origin_instance_uid",
		"hidden setup",
	} {
		assert.NotContains(t, lower, forbidden, msgAndArgs...)
	}
}

func TestNewRootCmdResetsGlobalFlagState(t *testing.T) {
	resetFlags(t)
	flags.Format = "json"
	flags.FormatValues = []string{"json"}
	flags.JSON = true
	flags.Agent = true
	flags.Workspace = "/tmp/leaked"

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"version"})

	require.NoError(t, cmd.Execute())
	assert.False(t, flags.JSON)
	assert.False(t, flags.Agent)
	assert.Empty(t, flags.FormatValues)
	assert.Empty(t, flags.Workspace)
}

// TestExitCodeFor_PureMapping pins the exit-code decision logic so a future
// refactor can't silently revert ExitUsage vs ExitInternal classification.
func TestExitCodeFor_PureMapping(t *testing.T) {
	assert.Equal(t, ExitUsage, exitCodeFor(assert.AnError, false),
		"cobra parse error (RunE never entered) maps to ExitUsage")
	assert.Equal(t, ExitInternal, exitCodeFor(assert.AnError, true),
		"plain RunE failure (runEEntered=true) maps to ExitInternal")
}

// TestRunEEntered_FalseOnUnknownCommand verifies cobra rejects an unknown
// command before PersistentPreRunE fires.
func TestRunEEntered_FalseOnUnknownCommand(t *testing.T) {
	resetRunEEntered(t)
	_, _, err := executeRootCapture(t, context.Background(), "this-command-does-not-exist")
	require.Error(t, err)
	assert.False(t, runEEntered, "PersistentPreRunE must not fire on unknown command")
	assert.Equal(t, ExitUsage, exitCodeFor(err, runEEntered))
}

// TestRunEEntered_FalseOnNoArgsViolation confirms the cobra.NoArgs validator
// on whoami short-circuits before PersistentPreRunE.
func TestRunEEntered_FalseOnNoArgsViolation(t *testing.T) {
	resetRunEEntered(t)
	_, _, err := executeRootCapture(t, context.Background(), "whoami", "unexpected-positional-arg")
	require.Error(t, err)
	assert.False(t, runEEntered, "NoArgs rejection must short-circuit before PersistentPreRunE")
	assert.Equal(t, ExitUsage, exitCodeFor(err, runEEntered))
}

// TestRunEEntered_TrueOnSuccessfulRunE confirms PersistentPreRunE fires when
// args/flags are valid. whoami needs no daemon, so it's a clean witness.
func TestRunEEntered_TrueOnSuccessfulRunE(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, _, err := executeRootCapture(t, context.Background(), "whoami", "--as", "test-actor")
	require.NoError(t, err)
	assert.True(t, runEEntered, "PersistentPreRunE should fire before whoami's RunE")
}

// TestRoot_Plan2VerbsAdvertised pins the top-level verbs against
// cmd.Commands() (not raw help substrings) so a missed registration is
// caught at test time rather than at the help text. The eight dedicated
// link-editing commands were retired by kata#1; relationships now flow
// through `kata edit --parent / --blocks / --blocked-by / --related`
// (and matching --remove-* flags).
func TestRoot_Plan2VerbsAdvertised(t *testing.T) {
	registered := rootSubcommands()
	for _, verb := range []string{
		"label", "labels",
		"assign", "unassign",
		"ready",
	} {
		_, ok := registered[verb]
		assert.Truef(t, ok, "root must register subcommand %q", verb)
	}
}

// TestRoot_RetiredCommandsAreGone pins the deletion of the 8 dedicated
// relationship-editing commands. If any of these come back as a registered
// subcommand, this test surfaces it.
func TestRoot_RetiredCommandsAreGone(t *testing.T) {
	registered := rootSubcommands()
	for _, retired := range []string{
		"link", "unlink", "parent", "unparent",
		"block", "unblock", "relate", "unrelate",
	} {
		_, ok := registered[retired]
		assert.Falsef(t, ok, "command %q should have been retired in kata#1", retired)
	}
}

// TestRoot_Plan3VerbsAdvertised mirrors the Plan 2 advertise check for the
// search-and-destroy verbs Plan 3 introduces. A future regression that
// drops a `subs` line will surface here, before it bites a user at the help.
func TestRoot_Plan3VerbsAdvertised(t *testing.T) {
	registered := rootSubcommands()
	for _, verb := range []string{"delete", "restore", "purge", "search"} {
		_, ok := registered[verb]
		assert.Truef(t, ok, "root must register subcommand %q", verb)
	}
}

func TestRoot_LeaseVerbsAreFederationScoped(t *testing.T) {
	registered := rootSubcommands()
	// `claim` lives at root for the simple ownership claim (PR #49); the
	// federation write-lease verbs stay under `federation lease`.
	_, ok := registered["release"]
	assert.False(t, ok, "root must not register federation lease command \"release\"")
	federation, ok := registered["federation"]
	require.True(t, ok, "root must register federation")
	lease, _, err := federation.Find([]string{"lease"})
	require.NoError(t, err)
	assert.Equal(t, "lease", lease.Name())
}

func TestRoot_QuickstartAdvertised(t *testing.T) {
	registered := rootSubcommands()
	quickstart, ok := registered["quickstart"]
	require.True(t, ok, "root must register quickstart")
	assert.Contains(t, quickstart.Aliases, "agent-instructions")
}

func TestHelp_RefFlagsDoNotAdvertiseLegacyNumbers(t *testing.T) {
	for _, args := range [][]string{
		{"create", "--help"},
		{"edit", "--help"},
	} {
		stdout, _, err := executeRootCapture(t, context.Background(), args...)
		require.NoError(t, err)
		assert.NotContains(t, stdout, "#N")
		assert.NotContains(t, stdout, "bare number")
		assert.NotContains(t, stdout, "8+ char prefix")
		assert.Contains(t, stdout, "short_id")
		assert.Contains(t, stdout, "kata#abc4")
	}
}

// resetRunEEntered restores the package-level sentinel via t.Cleanup so tests
// don't leak state across the shuffled order.
func resetRunEEntered(t *testing.T) {
	t.Helper()
	saved := runEEntered
	runEEntered = false
	t.Cleanup(func() { runEEntered = saved })
}

// TestEmitError_JSONMode_ProducesParseableEnvelope confirms that under
// --json the error path emits a JSON envelope shaped after the
// daemon's ErrorEnvelope plus client-side `kind` and `exit_code` fields,
// instead of "kata: <message>". This is the contract gap that hammer
// finding #3 flagged.
func TestEmitError_JSONMode_ProducesParseableEnvelope(t *testing.T) {
	cli := &cliError{
		Message:  "issue not found",
		Code:     "issue_not_found",
		Kind:     kindNotFound,
		ExitCode: ExitNotFound,
	}
	var buf bytes.Buffer
	emitError(&buf, cli, true, true)
	got := parseErrorEnvelope(t, buf.Bytes())
	assert.Equal(t, "not_found", got.Error.Kind)
	assert.Equal(t, "issue_not_found", got.Error.Code)
	assert.Equal(t, "issue not found", got.Error.Message)
	assert.Equal(t, ExitNotFound, got.Error.ExitCode)
}

// TestEmitError_HumanMode_StillPrintsKataPrefix locks the legacy
// human path so a future refactor doesn't break scripts grepping
// stderr for "kata:".
func TestEmitError_HumanMode_StillPrintsKataPrefix(t *testing.T) {
	cli := &cliError{
		Message: "title must not be empty", Kind: kindValidation,
		ExitCode: ExitValidation,
	}
	var buf bytes.Buffer
	emitError(&buf, cli, false, true)
	assert.Contains(t, buf.String(), "kata: title must not be empty")
}

func TestEmitError_AgentMode_CommandError(t *testing.T) {
	cli := &cliError{
		Message:  "comment body is required",
		Code:     "comment_body_required",
		Kind:     kindValidation,
		ExitCode: ExitValidation,
	}
	var buf bytes.Buffer
	emitAgentError(&buf, "comment", cli)
	assert.Equal(t, "ERR comment validation: comment body is required\n", buf.String())
}

func TestEmitError_AgentMode_UnknownCommandUsesKata(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--agent", "cretae")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "ERR kata usage:"),
		"stderr should start with agent usage error, got %q", stderr)
}

func TestEmitError_AgentMode_ParseErrorSingleLine(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--agent", "cretae")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "ERR kata usage:"),
		"stderr should start with agent usage error, got %q", stderr)
	assert.Equal(t, 1, strings.Count(stderr, "\n"), "agent error must be one physical line: %q", stderr)
	assert.NotContains(t, stderr, "Did you mean")
}

func TestEmitError_OutputModeErrorPrecedesUnknownCommand(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--format", "xml", "cretae")
	require.Error(t, err)
	assert.Contains(t, stderr, "unsupported output format")
	assert.NotContains(t, stderr, "unknown command")
}

func TestEmitError_AgentAliasWithInvalidFormatStillUsesAgent(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--agent", "--format", "xml", "version")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "ERR version usage:"),
		"stderr should use agent mode, got %q", stderr)
	assert.Contains(t, stderr, "unsupported output format")
}

func TestEmitError_OutputModeConflictPrecedesUnknownCommand(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--json", "--agent", "cretae")
	require.Error(t, err)
	assert.Contains(t, stderr, "conflicting output modes")
	assert.NotContains(t, stderr, "unknown command")
}

func TestOutputMode_RepeatedFormatConflicts(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	stdout, stderr, err := executeRootCapture(t, context.Background(),
		"--format", "human", "--format", "json", "version")
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "conflicting output modes")
}

func TestEmitError_RawModeScanParsesJSONTrueValue(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--json=true", "cretae")
	require.Error(t, err)
	got := parseErrorEnvelope(t, []byte(stderr))
	assert.Equal(t, "usage", got.Error.Kind)
	assert.Contains(t, got.Error.Message, "unknown command")
}

func TestEmitError_RawModeScanParsesAgentTrueValue(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--agent=true", "cretae")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "ERR kata usage:"),
		"stderr should use agent mode, got %q", stderr)
}

func TestEmitError_RawModeScanParsesJSONFalseValue(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--json=false", "cretae")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "kata:"), "stderr should stay human, got %q", stderr)
}

func TestEmitError_RawModeScanParsesAgentFalseValue(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--agent=false", "cretae")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "kata:"), "stderr should stay human, got %q", stderr)
}

func TestEmitError_RawModeScanSkipsWorkspaceFormatValue(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--workspace", "--format", "show")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "kata:"), "stderr should stay human, got %q", stderr)
	assert.NotContains(t, stderr, "unsupported output format")
}

func TestEmitError_RawModeScanSkipsWorkspaceAgentValue(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--workspace", "--agent", "show")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "kata:"), "stderr should stay human, got %q", stderr)
	assert.NotContains(t, stderr, "ERR ")
}

func TestEmitError_RawModeScanSkipsCreateBodyJSONValue(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "create", "--body", "--json")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "kata:"), "stderr should stay human, got %q", stderr)
	assert.Falsef(t, strings.HasPrefix(stderr, "{"), "stderr should not be JSON, got %q", stderr)
}

func TestEmitError_InvalidCommandPathDoesNotSwallowJSON(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "typo", "create", "--body", "--json")
	require.Error(t, err)
	got := parseErrorEnvelope(t, []byte(stderr))
	assert.Equal(t, "usage", got.Error.Kind)
	assert.Contains(t, got.Error.Message, "unknown command")
}

func TestEmitError_RawModeScanSkipsCreateBodyAgentValue(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "create", "--body", "--agent")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "kata:"), "stderr should stay human, got %q", stderr)
	assert.NotContains(t, stderr, "ERR ")
}

func TestEmitError_AgentMode_CommandArgErrorUsesLeafCommand(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, context.Background(), "--agent", "show")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "ERR show usage:"),
		"stderr should start with agent usage error for show, got %q", stderr)
}

// TestEmitError_NonCliError_SynthesizesEnvelope confirms a plain
// error (e.g. a network failure that escaped the cliError wrap)
// still gets a uniform JSON envelope when --json is set, with the
// kind inferred from the runEReached heuristic.
func TestEmitError_NonCliError_SynthesizesEnvelope(t *testing.T) {
	plain := errors.New("connection refused")
	var buf bytes.Buffer
	emitError(&buf, plain, true, true) // runEReached=true → ExitInternal/internal
	got := parseErrorEnvelope(t, buf.Bytes())
	assert.Equal(t, "internal", got.Error.Kind)
	assert.Equal(t, "connection refused", got.Error.Message)
	assert.Equal(t, ExitInternal, got.Error.ExitCode)
}

// TestKindForExit pins the exit-code → kind mapping so additions to
// the exit-code table can't silently drift.
func TestKindForExit(t *testing.T) {
	cases := map[int]errKind{
		ExitOK:            kindInternal, // 0 isn't an error path; defaults
		ExitInternal:      kindInternal,
		ExitUsage:         kindUsage,
		ExitValidation:    kindValidation,
		ExitNotFound:      kindNotFound,
		ExitConflict:      kindConflict,
		ExitConfirm:       kindConfirm,
		ExitDaemonUnavail: kindDaemonUnavail,
	}
	for code, want := range cases {
		assert.Equalf(t, want, kindForExit(code),
			"kindForExit(%d) = %q, want %q", code, kindForExit(code), want)
	}
}

// TestKindForStatus pins the HTTP-status → kind mapping (used by
// apiErrFromBody when the daemon returns an error envelope).
func TestKindForStatus(t *testing.T) {
	assert.Equal(t, kindValidation, kindForStatus(400))
	assert.Equal(t, kindNotFound, kindForStatus(404))
	assert.Equal(t, kindConflict, kindForStatus(409))
	assert.Equal(t, kindConfirm, kindForStatus(412))
	assert.Equal(t, kindInternal, kindForStatus(500))
}

// TestHealth_DoesNotAutoStartDaemon covers hammer-test finding #1:
// `kata health` used to call ensureDaemon, which auto-starts a
// daemon if none is running. A health probe should report the
// system's actual state, not paper over it. After the fix, health
// uses discoverDaemon and returns a kindDaemonUnavail cliError when
// no daemon is found.
func TestHealth_DoesNotAutoStartDaemon(t *testing.T) {
	// We can't easily test "no daemon" directly because tests share a
	// daemon namespace, but we CAN verify the discoverDaemon helper
	// returns a kindDaemonUnavail cliError when discovery fails. The
	// caller (health.RunE) propagates the error verbatim, so this
	// test pins the helper's contract.
	resetFlags(t)
	// Empty context (no BaseURLKey) + a fresh KATA_HOME guarantees
	// no daemon discovery succeeds.
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "")
	_, err := discoverDaemon(context.Background())
	require.Error(t, err)
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ExitDaemonUnavail, ce.ExitCode)
	assert.Equal(t, kindDaemonUnavail, ce.Kind)
	assert.Contains(t, ce.Message, "no daemon running",
		"hint must point the user at the right action")
}

// TestHealth_HonorsKataServer ensures a configured remote URL is
// probed by discoverDaemon. Without this, `kata health` ignores
// KATA_SERVER and reports either a stale local daemon or "no daemon
// running" — both of which contradict the user's explicit selection.
func TestHealth_HonorsKataServer(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_HOME", t.TempDir())

	env := testenv.New(t)
	t.Setenv("KATA_SERVER", env.URL)

	url, err := discoverDaemon(context.Background())
	require.NoError(t, err)
	assert.Equal(t, env.URL, url,
		"discoverDaemon must return the KATA_SERVER URL when it's reachable")
}

// TestList_ShowsOwnerInParens covers hammer-test #10: list and ready
// used to disagree on what the trailing "(...)" cell meant — list
// printed Author, ready printed Owner. List now matches ready by
// printing Owner; unowned issues render as "(unowned)" so the cell
// is never empty.
func TestList_ShowsOwnerInParens(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	body := []byte(`{"actor":"x","title":"T","owner":"alice"}`)
	resp, err := http.Post(env.URL+"/api/v1/projects/"+itoa(pid)+"/issues",
		"application/json", bytes.NewReader(body)) //nolint:gosec,noctx // test-only loopback
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	_ = resp.Body.Close()

	out := runCLI(t, env, dir, "list")
	assert.Contains(t, out, "(alice)", "list must show owner in parens")
	assert.NotContains(t, out, "(x)",
		"list must not show author in parens (would disagree with ready)")
}

// TestNegativePositional_ProducesUsefulError covers hammer-test #9:
// a positional that looks like a negative integer (kata show -1)
// used to produce "unknown shorthand flag: '1' in -1" — useless.
// Now translated into a kindUsage cliError pointing the user at
// the `--` separator workaround.
func TestNegativePositional_ProducesUsefulError(t *testing.T) {
	for _, args := range [][]string{
		{"show", "-1"},
		{"delete", "-1"},
		{"edit", "-1", "--title", "x"},
	} {
		_, err := runCmdOutput(t, nil, args...)
		require.Errorf(t, err, "args %v should error", args)
		ce := requireCLIError(t, err, ExitUsage)
		assert.Equal(t, kindUsage, ce.Kind)
		assert.Contains(t, ce.Message, "--",
			"useful error must mention the `--` separator workaround")
	}
}
