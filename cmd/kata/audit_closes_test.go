package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wontfixMessage is the canned ≥60-char wontfix message used by the
// audit tests. Centralizing it keeps the per-test lines under 100
// chars (which the close prose alone otherwise blows past).
const wontfixMessage = "Decided not to fix; out of scope for this milestone and product direction."

// TestAuditCloses_ListsAllClosesInWindow verifies the happy path: every
// close event in the default (open) window surfaces in the JSON output
// with its reason intact.
func TestAuditCloses_ListsAllClosesInWindow(t *testing.T) {
	env, dir, pid, refOne := setupWorkspaceWithIssue(t, "issue one")
	refTwo := createIssue(t, env, pid, "issue two")
	runCLI(t, env, dir, "close", refOne, "--done",
		"--message", "Fixed first issue and ran the auth tests.",
		"--commit", "abc1234")
	runCLI(t, env, dir, "close", refTwo, "--wontfix",
		"--message", wontfixMessage)

	out := runCLI(t, env, dir, "audit", "closes", "--json")
	assert.Contains(t, out, `"issue":"`+refOne+`"`)
	assert.Contains(t, out, `"issue":"`+refTwo+`"`)
	assert.Contains(t, out, `"reason":"done"`)
	assert.Contains(t, out, `"reason":"wontfix"`)
}

// TestAuditCloses_TextOutputRendersRows pins the non-JSON path: a row
// per close must appear under the header. Regression test for an
// unmarshal mismatch where the CLI decoded into the full
// AuditClosesResponse shape (with nested Body.Rows) while huma emits
// the body content directly, leaving Rows empty and the table blank.
func TestAuditCloses_TextOutputRendersRows(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "issue one")
	runCLI(t, env, dir, "close", ref, "--done",
		"--message", "Fixed the issue and ran the auth tests thoroughly.",
		"--commit", "abc1234")

	out := runCLI(t, env, dir, "audit", "closes")
	assert.Contains(t, out, "TIME")
	assert.Contains(t, out, ref,
		"text output must include the closed issue's short_id")
	assert.Contains(t, out, "done",
		"text output must include the close reason")
}

func TestAuditCloses_AgentOutputShape(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "issue one")
	runCLIAs(t, env, dir, "tester", "close", ref, "--done",
		"--message", "Fixed the issue and ran the auth tests thoroughly.",
		"--commit", "abc1234")

	out := runCLI(t, env, dir, "--agent", "audit", "closes")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "agent audit should include header and rows: %q", out)
	assert.Regexp(t, `^OK audit count=\d+`, lines[0])
	assert.Contains(t, lines[1], "actor=tester")
	assert.Contains(t, lines[1], "issue="+ref)
	assert.Contains(t, lines[1], "reason=done")
	assert.Contains(t, lines[1], "evidence=commit")
	assert.NotContains(t, out, "\x1b", "agent output must not contain ANSI escape bytes")
}

// TestAuditCloses_FilterByActor verifies --actor narrows results to a
// single actor's closes.
func TestAuditCloses_FilterByActor(t *testing.T) {
	env, dir, pid, refOne := setupWorkspaceWithIssue(t, "issue one")
	refTwo := createIssue(t, env, pid, "issue two")
	runCLIAs(t, env, dir, "alice", "close", refOne, "--done",
		"--message", "Fixed the issue alice owned and ran the auth tests.",
		"--commit", "abc1234")
	runCLIAs(t, env, dir, "bob", "close", refTwo, "--done",
		"--message", "Fixed the issue bob owned and ran the cache tests.",
		"--commit", "def5678")

	out := runCLI(t, env, dir, "audit", "closes", "--actor", "alice", "--json")
	assert.Contains(t, out, `"actor":"alice"`)
	assert.NotContains(t, out, `"actor":"bob"`)
}

// TestAuditCloses_FilterByReason verifies --reason narrows to a single
// close reason (here, the wontfix close is filtered out).
func TestAuditCloses_FilterByReason(t *testing.T) {
	env, dir, pid, refOne := setupWorkspaceWithIssue(t, "issue one")
	refTwo := createIssue(t, env, pid, "issue two")
	runCLI(t, env, dir, "close", refOne, "--done",
		"--message", "Fixed first issue and ran the auth tests.",
		"--commit", "abc1234")
	runCLI(t, env, dir, "close", refTwo, "--wontfix",
		"--message", wontfixMessage)

	out := runCLI(t, env, dir, "audit", "closes", "--reason", "done", "--json")
	assert.Contains(t, out, `"issue":"`+refOne+`"`)
	assert.NotContains(t, out, `"issue":"`+refTwo+`"`)
	assert.Contains(t, out, `"reason":"done"`)
	assert.NotContains(t, out, `"reason":"wontfix"`)
}

// TestAuditCloses_WontfixHasNoNoEvidenceFlag verifies that a wontfix
// close (which is admissible without evidence) does NOT get the
// "no-evidence" flag — that flag is reserved for closes that should
// have carried evidence and didn't.
func TestAuditCloses_WontfixHasNoNoEvidenceFlag(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "issue one")
	runCLI(t, env, dir, "close", ref, "--wontfix",
		"--message", wontfixMessage)

	out := runCLI(t, env, dir, "audit", "closes", "--json")
	assert.Contains(t, out, `"reason":"wontfix"`)
	assert.NotContains(t, out, `"no-evidence"`)
}

// TestAuditCloses_FilterByNoEvidenceEmptyByDefault verifies that when
// every close in the window carries evidence, --no-evidence returns no
// rows (the projection's Rows field is an empty list rather than
// missing or nil).
func TestAuditCloses_FilterByNoEvidenceEmptyByDefault(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "issue one")
	runCLI(t, env, dir, "close", ref, "--done",
		"--message", "Fixed first issue and ran the auth tests.",
		"--commit", "abc1234")

	out := runCLI(t, env, dir, "audit", "closes", "--no-evidence", "--json")
	assert.NotContains(t, out, `"issue":"`+ref+`"`)
	assert.Contains(t, out, `"rows":[]`)
}

// TestAuditCloses_EvidenceTypesSurfaceInRow verifies that the
// evidence_types projection lists the evidence kinds attached to a
// close (here, "commit" from --commit abc1234).
func TestAuditCloses_EvidenceTypesSurfaceInRow(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "issue one")
	runCLI(t, env, dir, "close", ref, "--done",
		"--message", "Fixed first issue and ran the auth tests.",
		"--commit", "abc1234")

	out := runCLI(t, env, dir, "audit", "closes", "--json")
	assert.Contains(t, out, `"evidence_types":["commit"]`)
}

// TestAuditCloses_FilterByParent verifies --parent narrows to closes
// of children of the given parent issue.
func TestAuditCloses_FilterByParent(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	childOfParent := createIssue(t, env, pid, "child of parent")
	runCLI(t, env, dir, "edit", childOfParent, "--parent", parent)
	unrelated := createIssue(t, env, pid, "unrelated issue")
	runCLI(t, env, dir, "close", childOfParent, "--done",
		"--message", "Fixed the child of the parent issue and ran the unit tests.",
		"--commit", "abc1234")
	runCLI(t, env, dir, "close", unrelated, "--done",
		"--message", "Fixed the unrelated issue and ran the integration tests.",
		"--commit", "def5678")

	out := runCLI(t, env, dir, "audit", "closes", "--parent", parent, "--json")
	assert.Contains(t, out, `"issue":"`+childOfParent+`"`)
	assert.NotContains(t, out, `"issue":"`+unrelated+`"`)
}

// TestAuditCloses_ParentFrozenAtCloseTime pins close-time parent
// capture: the audit row's parent must reflect the parent at the
// moment of close, not whatever the live `links` table happens to
// hold when the audit runs. Without the frozen parent_short_id in
// the event payload, an agent could close children under parent A
// and then reparent them to B to shift their audit rows out of
// `audit closes --parent A`.
func TestAuditCloses_ParentFrozenAtCloseTime(t *testing.T) {
	env, dir, pid, parentA := setupWorkspaceWithIssue(t, "parent A")
	parentB := createIssue(t, env, pid, "parent B")
	child := createIssue(t, env, pid, "child of A")
	runCLI(t, env, dir, "edit", child, "--parent", parentA)
	runCLI(t, env, dir, "close", child, "--done",
		"--message", "Fixed the child of parent A and ran the unit tests.",
		"--commit", "abc1234")

	out := runCLI(t, env, dir, "audit", "closes", "--parent", parentA, "--json")
	assert.Contains(t, out, `"issue":"`+child+`"`,
		"audit before reparent must include the close under parent A")

	// Reparent the now-closed child onto B and re-query. The original
	// close happened under A — the audit row must still attribute it
	// to A, and the --parent B filter must NOT pick it up.
	runCLI(t, env, dir, "reopen", child)
	runCLI(t, env, dir, "edit", child, "--remove-parent", parentA)
	runCLI(t, env, dir, "edit", child, "--parent", parentB)

	outA := runCLI(t, env, dir, "audit", "closes", "--parent", parentA, "--json")
	assert.Contains(t, outA, `"issue":"`+child+`"`,
		"audit after reparent must still attribute the close to A")
	outB := runCLI(t, env, dir, "audit", "closes", "--parent", parentB, "--json")
	assert.NotContains(t, outB, `"issue":"`+child+`"`,
		"audit after reparent must not migrate the historical close onto B")
}

// TestAuditCloses_ThrottledFlagDoesNotBleedAcrossReopenCycles pins
// the single-pass marker rule: a close.throttled event marks the next
// matching issue.closed and is then consumed. A later reopen → close
// for the same (issue, actor) starts a fresh history and must NOT
// inherit the prior throttle flag — otherwise an all-time audit window
// would keep stamping "throttled" on closes that have nothing to do
// with the original refusal.
func TestAuditCloses_ThrottledFlagDoesNotBleedAcrossReopenCycles(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	childA := createIssue(t, env, pid, "child a")
	childB := createIssue(t, env, pid, "child b")
	runCLI(t, env, dir, "edit", childA, "--parent", parent)
	runCLI(t, env, dir, "edit", childB, "--parent", parent)
	msg := "Verified end-to-end and confirmed no regressions in the test suite."
	// First close cycle: A closes, B trips the repeated-message guard,
	// then B succeeds with distinct prose. B's successful close should
	// be flagged throttled.
	runCLIAs(t, env, dir, "agent-a", "close", childA, "--done",
		"--message", msg, "--commit", "abc1234")
	_, _, _ = runCLIWithErr(t, env, dir, "close", childB, "--as", "agent-a",
		"--done", "--message", msg, "--commit", "abc1234")
	runCLIAs(t, env, dir, "agent-a", "close", childB, "--done",
		"--message", "Different prose for child B; independent verification path.",
		"--commit", "def5678")
	// Second cycle on B: reopen, then close again with a fresh message.
	// This close happens AFTER the prior close.throttled event in the
	// window, but it belongs to a separate cycle and must NOT carry the
	// throttled flag.
	runCLIAs(t, env, dir, "agent-a", "reopen", childB)
	runCLIAs(t, env, dir, "agent-a", "close", childB, "--done",
		"--message", "Second close cycle on child B after reopening for cleanup.",
		"--commit", "ff00aa1")

	out := runCLI(t, env, dir, "audit", "closes", "--actor", "agent-a", "--json")
	type row struct {
		Issue   string   `json:"issue"`
		Message string   `json:"message"`
		Flags   []string `json:"flags"`
	}
	var got struct {
		Rows []row `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	var seenFlagged, seenClean bool
	for _, r := range got.Rows {
		if r.Issue != childB {
			continue
		}
		hasThrottled := false
		for _, f := range r.Flags {
			if f == "throttled" {
				hasThrottled = true
			}
		}
		if strings.HasPrefix(r.Message, "Different prose") {
			seenFlagged = hasThrottled
		}
		if strings.HasPrefix(r.Message, "Second close cycle") {
			seenClean = !hasThrottled
		}
	}
	assert.True(t, seenFlagged,
		"the close that retried past the throttle must carry the throttled flag")
	assert.True(t, seenClean,
		"a later reopen→close cycle must not inherit the prior throttle flag")
}

// TestAuditCloses_FilterByParentUID pins that --parent accepts any
// ref form the resolver accepts (bare short_id, full UID, or
// qualified `project#short_id`), not just the rendered short_id. A
// caller using the parent's UID (or pasting a `kata show --json`
// ULID) should reach the same rows as the short_id form.
func TestAuditCloses_FilterByParentUID(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	child := createIssue(t, env, pid, "child of parent")
	runCLI(t, env, dir, "edit", child, "--parent", parent)
	runCLI(t, env, dir, "close", child, "--done",
		"--message", "Fixed the child of the parent issue and ran the unit tests.",
		"--commit", "abc1234")

	// Resolve the parent's full UID via show --json so we can drive
	// --parent <UID>.
	showOut := runCLI(t, env, dir, "show", parent, "--json")
	var shown struct {
		Issue struct {
			UID string `json:"uid"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(showOut), &shown))
	require.NotEmpty(t, shown.Issue.UID, "show --json must surface the parent's UID")

	out := runCLI(t, env, dir, "audit", "closes", "--parent", shown.Issue.UID, "--json")
	assert.Contains(t, out, `"issue":"`+child+`"`,
		"--parent should accept the parent's UID and surface the close")
}

// TestAuditCloses_FilterByParent_SoftDeletedParent pins that the
// --parent filter still works when the parent has been soft-deleted
// after the child was closed. Audit is a historical view; using
// IncludeDeletedNo on the parent resolver silently dropped these
// rows from the response.
func TestAuditCloses_FilterByParent_SoftDeletedParent(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	child := createIssue(t, env, pid, "child of parent")
	runCLI(t, env, dir, "edit", child, "--parent", parent)
	runCLI(t, env, dir, "close", child, "--done",
		"--message", "Fixed the child of the parent issue and ran the unit tests.",
		"--commit", "abc1234")
	// Soft-delete the parent. The historical close of the child must
	// still surface under --parent <parent_ref> because the close
	// event was recorded when the parent was alive.
	runCLI(t, env, dir, "delete", parent, "--force", "--confirm", "DELETE kata#"+parent)

	out := runCLI(t, env, dir, "audit", "closes", "--parent", parent, "--json")
	assert.Contains(t, out, `"issue":"`+child+`"`,
		"audit --parent must still match after the parent is soft-deleted")
}

// TestAuditCloses_ThrottledFlag_DifferentActorEndsCycle pins the
// per-actor marker rule: any close of the issue ends the throttle
// cycle, but only the throttled actor's close gets the flag. An
// intervening close by a different actor must NOT inherit the flag,
// and a later close by the originally-throttled actor (after a
// reopen) must NOT inherit a stale flag from before the intervening
// close.
func TestAuditCloses_ThrottledFlag_DifferentActorEndsCycle(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	childA := createIssue(t, env, pid, "child a")
	childB := createIssue(t, env, pid, "child b")
	runCLI(t, env, dir, "edit", childA, "--parent", parent)
	runCLI(t, env, dir, "edit", childB, "--parent", parent)
	msg := "Verified end-to-end and confirmed no regressions in tests."
	// Throttle agent-a on childB by reusing childA's close message.
	runCLIAs(t, env, dir, "agent-a", "close", childA, "--done",
		"--message", msg, "--commit", "abc1234")
	_, _, _ = runCLIWithErr(t, env, dir, "close", childB, "--as", "agent-a",
		"--done", "--message", msg, "--commit", "abc1234")
	// agent-b (a different actor) closes childB. The pending throttle
	// for (childB, agent-a) ends, but agent-b's close must NOT be
	// flagged throttled.
	runCLIAs(t, env, dir, "agent-b", "close", childB, "--done",
		"--message", "Different actor closing child B with fresh prose.",
		"--commit", "def5678")
	// Reopen and let agent-a close it later. agent-a's later close
	// must NOT inherit the stale throttle marker (agent-b already
	// ended that cycle).
	runCLIAs(t, env, dir, "agent-b", "reopen", childB)
	runCLIAs(t, env, dir, "agent-a", "close", childB, "--done",
		"--message", "Second cycle on child B; agent-a closing after reopen.",
		"--commit", "ff00aa1")

	out := runCLI(t, env, dir, "audit", "closes", "--json")
	type row struct {
		Issue   string   `json:"issue"`
		Actor   string   `json:"actor"`
		Message string   `json:"message"`
		Flags   []string `json:"flags"`
	}
	var got struct {
		Rows []row `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	hasThrottled := func(flags []string) bool {
		for _, f := range flags {
			if f == "throttled" {
				return true
			}
		}
		return false
	}
	var seenDifferentActor, seenSecondCycle bool
	for _, r := range got.Rows {
		if r.Issue != childB {
			continue
		}
		switch {
		case strings.HasPrefix(r.Message, "Different actor"):
			seenDifferentActor = true
			assert.False(t, hasThrottled(r.Flags),
				"agent-b's intervening close must not inherit agent-a's throttle marker")
		case strings.HasPrefix(r.Message, "Second cycle"):
			seenSecondCycle = true
			assert.False(t, hasThrottled(r.Flags),
				"agent-a's later close after the intervening close must start a fresh cycle")
		}
	}
	assert.True(t, seenDifferentActor,
		"audit must surface the intervening close row to validate it against the throttle flag")
	assert.True(t, seenSecondCycle,
		"audit must surface the post-reopen close row to validate it against the throttle flag")
}

// TestAuditCloses_ActorFilterDoesNotHideThrottleEndingClose pins that
// pushing --actor into the SQL query would hide intervening closes by
// other actors and cause the throttle marker to bleed: agent-a is
// throttled, agent-b's intervening close ends that cycle, agent-a
// closes again after a reopen. With --actor=agent-a the SQL-only
// filter would drop agent-b's close from the event stream and leave
// agent-a's marker pending, flagging the later legitimate close.
// Lifting --actor to the row-emit pass keeps the marker walk
// consistent regardless of which actor the caller filters on.
func TestAuditCloses_ActorFilterDoesNotHideThrottleEndingClose(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	childA := createIssue(t, env, pid, "child a")
	childB := createIssue(t, env, pid, "child b")
	runCLI(t, env, dir, "edit", childA, "--parent", parent)
	runCLI(t, env, dir, "edit", childB, "--parent", parent)
	msg := "Verified end-to-end and confirmed no regressions in tests."
	runCLIAs(t, env, dir, "agent-a", "close", childA, "--done",
		"--message", msg, "--commit", "abc1234")
	_, _, _ = runCLIWithErr(t, env, dir, "close", childB, "--as", "agent-a",
		"--done", "--message", msg, "--commit", "abc1234")
	runCLIAs(t, env, dir, "agent-b", "close", childB, "--done",
		"--message", "Different actor closing child B with fresh prose.",
		"--commit", "def5678")
	runCLIAs(t, env, dir, "agent-b", "reopen", childB)
	runCLIAs(t, env, dir, "agent-a", "close", childB, "--done",
		"--message", "Second cycle on child B; agent-a closing after reopen.",
		"--commit", "ff00aa1")

	// With --actor=agent-a, the agent-b close is filtered out of the
	// output but MUST still be visible to the throttle-marker pass.
	// agent-a's second close (post-reopen) must NOT inherit the flag.
	out := runCLI(t, env, dir, "audit", "closes", "--actor", "agent-a", "--json")
	type row struct {
		Issue   string   `json:"issue"`
		Actor   string   `json:"actor"`
		Message string   `json:"message"`
		Flags   []string `json:"flags"`
	}
	var got struct {
		Rows []row `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	var seenSecondCycle bool
	for _, r := range got.Rows {
		if r.Issue != childB || !strings.HasPrefix(r.Message, "Second cycle") {
			continue
		}
		seenSecondCycle = true
		for _, f := range r.Flags {
			assert.NotEqual(t, "throttled", f,
				"--actor must not hide agent-b's cycle-ending close from the throttle pass")
		}
	}
	assert.True(t, seenSecondCycle,
		"--actor=agent-a must still surface agent-a's later close")
}

// TestAuditCloses_FilterByParent_QualifiedAndUIDRefs pins that
// --parent accepts qualified refs (`project#short_id`) and full UIDs
// even when the parent is no longer resolvable, by falling back to
// the parsed short_id / UID against stored payload snapshots. Prior
// to the parser fallback, qualified refs to soft-deleted-then-purged
// parents silently returned no rows.
func TestAuditCloses_FilterByParent_QualifiedAndUIDRefs(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	child := createIssue(t, env, pid, "child of parent")
	runCLI(t, env, dir, "edit", child, "--parent", parent)
	runCLI(t, env, dir, "close", child, "--done",
		"--message", "Fixed the child of the parent issue and ran the unit tests.",
		"--commit", "abc1234")

	qualified := "kata#" + parent
	out := runCLI(t, env, dir, "audit", "closes", "--parent", qualified, "--json")
	assert.Contains(t, out, `"issue":"`+child+`"`,
		"--parent must accept qualified refs even before the parser fallback path matters")
}

// TestAuditCloses_FilterByParent_CrossProjectQualifierMatchesNothing
// pins that a qualified ref naming a different project than the
// audit's scoped project must NOT match same-suffix issues in the
// scoped project. The resolver-404 fallback previously kept
// parsedShortID from `other#abc4`, so an audit for project=kata
// with `--parent other#abc4` would surface closes under kata#abc4.
func TestAuditCloses_FilterByParent_CrossProjectQualifierMatchesNothing(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	child := createIssue(t, env, pid, "child of parent")
	runCLI(t, env, dir, "edit", child, "--parent", parent)
	runCLI(t, env, dir, "close", child, "--done",
		"--message", "Fixed the child of the parent issue and ran the unit tests.",
		"--commit", "abc1234")

	// Qualified ref naming a project that doesn't exist (or any
	// project other than the scoped one) must return zero rows —
	// the matching short_id suffix in the scoped project is
	// coincidental, not what the user asked for.
	mismatchedRef := "other-project#" + parent
	out := runCLI(t, env, dir, "audit", "closes", "--parent", mismatchedRef, "--json")
	assert.NotContains(t, out, `"issue":"`+child+`"`,
		"--parent <other-project>#<short> must not match same-suffix closes in the scoped project")
	assert.Contains(t, out, `"rows":[]`,
		"cross-project qualified ref must return an empty rows list")
}

// TestAuditCloses_ThrottledFlagIgnoresLaterThrottle pins the temporal
// rule: a close.throttled event whose id is GREATER than the successful
// close it could naively be matched against must not flag that close.
// The throttle there is a later retry against the same key, not a guard
// the original close tripped — flagging it would mislead a reviewer.
func TestAuditCloses_ThrottledFlagIgnoresLaterThrottle(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	siblingA := createIssue(t, env, pid, "sibling a")
	siblingB := createIssue(t, env, pid, "sibling b")
	runCLI(t, env, dir, "edit", siblingA, "--parent", parent)
	runCLI(t, env, dir, "edit", siblingB, "--parent", parent)
	// Establish a successful close on siblingB with message M so a later
	// attempt to close siblingA with the same message trips the
	// repeated-message guard. Event 1 ≈ close (siblingB, agent-a).
	msg := "Verified consistent state and confirmed no regressions in tests."
	runCLIAs(t, env, dir, "agent-a", "close", siblingB, "--done",
		"--message", msg,
		"--commit", "abc1234")
	// Close siblingA successfully with a distinct message. Event 2 ≈
	// close (siblingA, agent-a). This is the row that must NOT be
	// flagged throttled — the throttle later in the timeline is
	// unrelated to this close.
	runCLIAs(t, env, dir, "agent-a", "close", siblingA, "--done",
		"--message", "Different prose for sibling A; independent verification.",
		"--commit", "def5678")
	// Reopen siblingA so we can attempt to close it again. Event 3.
	runCLIAs(t, env, dir, "agent-a", "reopen", siblingA)
	// Attempt to close siblingA with the duplicate message M. The
	// repeated-message guard fires because siblingB's prior close used
	// the same msg under the same parent. Event 4 ≈ close.throttled
	// (siblingA, agent-a).
	_, _, _ = runCLIWithErr(t, env, dir, "close", siblingA, "--as", "agent-a",
		"--done",
		"--message", msg,
		"--commit", "abc1234")

	out := runCLI(t, env, dir, "audit", "closes", "--actor", "agent-a", "--json")
	// The successful close of siblingA (event 2) must NOT carry a
	// throttled flag — the only close.throttled for (siblingA, agent-a)
	// is event 4, which post-dates the close.
	type row struct {
		Issue string   `json:"issue"`
		Flags []string `json:"flags"`
	}
	var got struct {
		Rows []row `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	var seenA bool
	for _, r := range got.Rows {
		if r.Issue == siblingA {
			seenA = true
			for _, f := range r.Flags {
				assert.NotEqual(t, "throttled", f,
					"close of %s must not be flagged throttled by a later throttle", siblingA)
			}
		}
	}
	assert.True(t, seenA, "audit should include the close of %s", siblingA)
}

// TestAuditCloses_ThrottledFlagSurfaces verifies that a close that
// previously hit the repeated-message guard (and thus emitted a
// close.throttled event) is flagged "throttled" in the audit row once
// the same actor retries successfully with a different message.
func TestAuditCloses_ThrottledFlagSurfaces(t *testing.T) {
	env, dir, pid, parent := setupWorkspaceWithIssue(t, "parent issue")
	childA := createIssue(t, env, pid, "child a")
	childB := createIssue(t, env, pid, "child b")
	runCLI(t, env, dir, "edit", childA, "--parent", parent)
	runCLI(t, env, dir, "edit", childB, "--parent", parent)
	msg := "Schema review complete; table remains metadata-only and unchanged."
	runCLIAs(t, env, dir, "agent-a", "close", childA, "--audit-no-change",
		"--message", msg,
		"--evidence", "no-change-audit:metadata-only")
	// Second close with identical message trips the repeated-message
	// guard and emits a close.throttled event tagged with agent-a/childB.
	_, _, _ = runCLIWithErr(t, env, dir, "close", childB, "--as", "agent-a",
		"--audit-no-change",
		"--message", msg,
		"--evidence", "no-change-audit:metadata-only")
	// Retry with a distinct message — the close succeeds; the prior
	// close.throttled event for agent-a/childB stays in the audit window.
	runCLIAs(t, env, dir, "agent-a", "close", childB, "--audit-no-change",
		"--message", "Reviewed sibling B; same conclusion under a fresh narrative.",
		"--evidence", "no-change-audit:metadata-only")

	out := runCLI(t, env, dir, "audit", "closes", "--actor", "agent-a", "--json")
	assert.Contains(t, out, `"throttled"`)
}
