package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloseReopen_RoundTrip(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")

	out := runCLI(t, env, dir, "close", ref,
		"--reason", "wontfix",
		"--message", "Decided not to fix this; it doesn't match the current product direction.")
	assert.Contains(t, out, "closed")

	out = runCLI(t, env, dir, "reopen", ref)
	assert.Contains(t, out, "open")
}

func TestClose_AgentOutput(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")

	out := runCLI(t, env, dir, "--agent", "close", ref,
		"--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234")

	assert.Regexp(t, `(?m)^OK close \S+`, out)
	assert.Contains(t, out, "Status: closed")
	assert.Contains(t, out, "Reason: done")
	assert.Contains(t, out, "Evidence: commit:abc1234")
}

func TestClose_AgentDryRunSuppressesHumanBanner(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")

	stdout, stderr, err := runCLIWithErr(t, env, dir, "--agent", "close", ref,
		"--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234",
		"--dry-run")

	require.NoError(t, err)
	assert.Regexp(t, `(?m)^OK close \S+`, stdout)
	assert.Empty(t, stderr)
}

func TestReopen_AgentOutput(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")
	runCLI(t, env, dir, "close", ref,
		"--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234")

	resetFlags(t)
	out := runCLI(t, env, dir, "--agent", "reopen", ref)

	assert.Regexp(t, `(?m)^OK reopen \S+`, out)
	assert.Contains(t, out, "Status: open")
}

func TestCloseCmd_CanonicalDoneRequiresEvidence(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")

	_, stderr, err := runCLIWithErr(t, env, dir,
		"close", ref,
		"--reason", "done",
		"--message", "Fixed Safari callback double-submit and ran tests.")
	require.Error(t, err)
	assert.Contains(t, stderr, "evidence required")
}

func TestCloseCmd_CanonicalDoneWithCommitEvidence(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")

	out := runCLI(t, env, dir,
		"close", ref,
		"--reason", "done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--evidence", "commit:abc1234")
	assert.Contains(t, out, "closed")
}

func TestCloseCmd_SugarDoneWithCommit(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")
	out := runCLI(t, env, dir,
		"close", ref,
		"--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234")
	assert.Contains(t, out, "closed")
}

func TestCloseCmd_SugarDuplicateOf(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "test issue")
	target := createIssue(t, env, pid, "target")
	out := runCLI(t, env, dir,
		"close", ref,
		"--duplicate-of", target,
		"--message", "Same Safari race; merge there.")
	assert.Contains(t, out, "closed")
}

func TestCloseCmd_SugarConflictsWithCanonical(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")
	_, stderr, err := runCLIWithErr(t, env, dir,
		"close", ref,
		"--reason", "done", "--done",
		"--message", "Fixed it.",
		"--evidence", "commit:abc1234")
	require.Error(t, err)
	assert.Contains(t, stderr, "conflict")
}

// TestCloseCmd_MultipleSugarFlagsRejected pins that combining two reason
// sugar flags (e.g. --done --wontfix) is refused with a clear conflict
// error rather than silently picking the first match in source order.
func TestCloseCmd_MultipleSugarFlagsRejected(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")
	_, stderr, err := runCLIWithErr(t, env, dir,
		"close", ref,
		"--done", "--wontfix",
		"--message", "Fixed it.",
		"--commit", "abc1234")
	require.Error(t, err)
	assert.Contains(t, stderr, "conflict")
	assert.Contains(t, stderr, "done")
	assert.Contains(t, stderr, "wontfix")
}

func TestCloseCmd_SugarDuplicateEvidenceConflict(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "test issue")
	target := createIssue(t, env, pid, "target")
	_, stderr, err := runCLIWithErr(t, env, dir,
		"close", ref,
		"--duplicate-of", target,
		"--evidence", "duplicate-of:"+target,
		"--message", "Same Safari race; merge there.")
	require.Error(t, err)
	assert.Contains(t, stderr, "conflict")
}

func TestParseEvidenceFlags_RejectsEmptyDuplicateOf(t *testing.T) {
	_, err := parseEvidenceFlags([]string{"duplicate-of:"})
	require.Error(t, err)
}

func TestParseEvidenceFlags_RejectsEmptySupersededBy(t *testing.T) {
	_, err := parseEvidenceFlags([]string{"superseded-by:"})
	require.Error(t, err)
}

func TestParseEvidenceFlags_RejectsDuplicateReviewedPath(t *testing.T) {
	_, err := parseEvidenceFlags([]string{
		"reviewed-paths:internal/foo.go",
		"reviewed-paths:internal/foo.go",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate path")
}

func TestCloseCmd_SugarReviewedAndCanonicalReviewedPathConflict(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")
	_, stderr, err := runCLIWithErr(t, env, dir,
		"close", ref,
		"--audit-no-change",
		"--message", "Reviewed and concluded no change is needed for this path.",
		"--evidence", "no-change-audit:metadata-only",
		"--reviewed", "internal/foo.go",
		"--evidence", "reviewed-paths:internal/foo.go")
	require.Error(t, err)
	assert.Contains(t, stderr, "duplicate path")
}

// TestCloseCmd_EvidenceValueWithCommaIsPreserved pins that --evidence
// values containing commas survive cobra's flag parser intact. cobra's
// StringSliceVar would split "no-change-audit:Reviewed schemas, queries,
// and migrations" into three broken evidence items; the audit row's
// stored evidence must reflect the original prose.
func TestCloseCmd_EvidenceValueWithCommaIsPreserved(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")
	runCLI(t, env, dir,
		"close", ref,
		"--audit-no-change",
		"--message", "Reviewed; comma-laden rationale should round-trip cleanly through cobra parsing.",
		"--evidence", "no-change-audit:Reviewed schemas, queries, and migrations")

	show := runCLI(t, env, dir, "show", ref, "--json")
	assert.Contains(t, show, `"status":"closed"`,
		"comma-bearing --evidence value must not be split into invalid sub-items")
}

func TestCloseCmd_DryRunDoesNotMutate(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")
	out := runCLI(t, env, dir,
		"close", ref,
		"--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234",
		"--dry-run")
	assert.Contains(t, out, "dry-run")
	assert.Contains(t, out, "[open]")

	show := runCLI(t, env, dir, "show", ref, "--json")
	assert.Contains(t, show, `"status":"open"`)
}

// TestCloseCmd_AlreadyClosedIsIdempotent pins that a second close on an
// already-closed issue is a no-op: it returns successfully without
// triggering 429 throttles or 409 parent-completeness refusals based on
// current state. The close handler short-circuits before running the
// structural guards once the issue is in the target state.
//
// To prove the short-circuit really skips the guards, the test stages a
// scenario where the parent-close completeness guard WOULD fire on a
// fresh attempt: the parent is closed first, then a child is created
// after that close (linking back via --parent). Re-closing the parent
// would normally 409 with "open children", but the already-closed
// short-circuit must surface success instead. Shape validation still
// runs, so the retry uses a different valid reason+message to verify
// the short-circuit isn't gated on matching prior inputs.
func TestCloseCmd_AlreadyClosedIsIdempotent(t *testing.T) {
	env, dir, _, parent := setupWorkspaceWithIssue(t, "parent issue")
	runCLI(t, env, dir, "close", parent, "--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234")
	// Link an OPEN child to the (now closed) parent. A fresh close of
	// the parent would trip CheckParentCloseCompleteness and 409.
	runCLI(t, env, dir, "create", "open child of closed parent", "--parent", parent)

	// Second close attempt with a different reason and well-formed
	// message. If the short-circuit broke and the guard ran, this would
	// fail with "open children" 409.
	out := runCLI(t, env, dir, "close", parent, "--wontfix",
		"--message", "Decided not to pursue this after further architectural review of the trade-offs.")
	assert.Contains(t, out, "closed")

	// Status remains closed; the original close-reason is preserved.
	show := runCLI(t, env, dir, "show", parent, "--json")
	assert.Contains(t, show, `"status":"closed"`)
}

// TestCloseCmd_AlreadyClosed_RetryWithInvalidPayloadStillIdempotent pins
// that the already-closed short-circuit runs BEFORE substance / evidence
// validation. A retry that omits --message and --evidence (e.g., a
// caller replaying a stale request after a connection drop) must still
// surface success because the issue is already in the target state.
// Pre-fix the validator ran first and returned 400 even though no state
// transition was on the table.
func TestCloseCmd_AlreadyClosed_RetryWithInvalidPayloadStillIdempotent(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")
	runCLI(t, env, dir, "close", ref, "--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234")

	// Retry with empty message and no evidence. A first close with this
	// payload would 400 — but the issue is already closed, so the
	// handler must short-circuit ahead of ValidateCloseInput.
	out := runCLI(t, env, dir, "close", ref, "--done")
	assert.Contains(t, out, "closed",
		"already-closed retry with invalid payload must return success envelope")
}

func TestCloseCmd_HelpBannerNamesObligation(t *testing.T) {
	out := string(executeRoot(t, newRootCmd(), "close", "--help"))
	assert.Contains(t, out, "asserts that the work it describes is complete")
	assert.Contains(t, out, "do not close it")
	assert.Contains(t, out, "needs-review")
}

func TestCloseCmd_ErrorTextNamesAlternative(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "test issue")
	_, stderr, err := runCLIWithErr(t, env, dir,
		"close", ref, "--done",
		"--message", "Fixed Safari callback double-submit and ran tests.")
	require.Error(t, err)
	assert.Contains(t, stderr, "evidence required")
	assert.Contains(t, stderr, "needs-review")
}

// TestCloseAPI_TUISourceBypassesSubstanceAndEvidence pins that a
// close posted with source="tui" succeeds without --message or
// --evidence, so the interactive close keystroke isn't broken by the
// CLI substance/evidence gate. Posts directly to the daemon endpoint
// to mirror the wire shape internal/tui/client.Close uses.
func TestCloseAPI_TUISourceBypassesSubstanceAndEvidence(t *testing.T) {
	env, _, pid, ref := setupWorkspaceWithIssue(t, "issue one")
	body, err := json.Marshal(map[string]any{
		"actor":  "alice",
		"source": "tui",
	})
	require.NoError(t, err)
	resp, err := http.Post( //nolint:noctx,gosec // test-only loopback
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", env.URL, pid, ref),
		"application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode, "TUI close must succeed: %s", bs)
}

// TestCloseAPI_TUISourceWithNonDoneReasonStillValidates pins the
// scope of the source="tui" bypass: only reason="done" is exempt
// from substance / evidence validation. A caller setting
// source="tui" with reason="duplicate" or "superseded" must still
// pass the evidence-target check — otherwise an agent could forge
// the TUI origin to skip the duplicate-of / superseded-by guard and
// persist a corrupt audit row.
func TestCloseAPI_TUISourceWithNonDoneReasonStillValidates(t *testing.T) {
	env, _, pid, ref := setupWorkspaceWithIssue(t, "issue one")
	body, err := json.Marshal(map[string]any{
		"actor":  "agent",
		"source": "tui",
		"reason": "duplicate",
		// Deliberately omit duplicate-of evidence; the daemon must
		// refuse rather than bypass validation on the TUI claim.
	})
	require.NoError(t, err)
	resp, err := http.Post( //nolint:noctx,gosec // test-only loopback
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", env.URL, pid, ref),
		"application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode,
		"source=tui with reason=duplicate must still require duplicate-of evidence: %s", bs)
}

// TestCloseAPI_NonTUIRequiresSubstance is the symmetric guard: without
// source="tui", an empty message must surface as 400 validation.
func TestCloseAPI_NonTUIRequiresSubstance(t *testing.T) {
	env, _, pid, ref := setupWorkspaceWithIssue(t, "issue one")
	body, err := json.Marshal(map[string]any{
		"actor": "alice",
	})
	require.NoError(t, err)
	resp, err := http.Post( //nolint:noctx,gosec // test-only loopback
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", env.URL, pid, ref),
		"application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode,
		"agent close without substance must be refused: %s", bs)
}

// TestReopen_SingleRefWorks pins that kata reopen <ref> reopens a single
// closed issue back to open.
func TestReopen_SingleRefWorks(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "issue one")
	runCLI(t, env, dir, "close", ref, "--done",
		"--message", "Closed first issue after thorough review.",
		"--commit", "abc1234")

	out := runCLI(t, env, dir, "reopen", ref)
	assert.Contains(t, out, "open")

	show := runCLI(t, env, dir, "show", ref, "--json")
	assert.Contains(t, show, `"status":"open"`)
}

func TestClose_WithComment_AppendsComment(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "test issue")

	runCLI(t, env, dir, "close", ref,
		"--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234",
		"--comment", "fixed in abc1234")

	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1, "close --comment must append exactly one comment")
	assert.Equal(t, "fixed in abc1234", got.Comments[0].Body)
	assert.Equal(t, "closed", got.Issue.Status)
}

func TestReopen_WithComment_AppendsComment(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "test issue")
	runCLI(t, env, dir, "close", ref,
		"--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234")

	runCLI(t, env, dir, "reopen", ref, "--comment", "regressed")

	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "regressed", got.Comments[0].Body)
	assert.Equal(t, "open", got.Issue.Status)
}

func TestClose_EmptyComment_Rejected(t *testing.T) {
	env, dir := setupCLIEnv(t)
	ref := createIssueViaHTTP(t, env, dir, "x")

	_, err := runCLICapture(t, env, dir, "close", ref,
		"--done",
		"--message", "Fixed Safari callback double-submit and ran tests.",
		"--commit", "abc1234",
		"--comment", "   ")
	_ = requireCLIError(t, err, ExitValidation)
}
