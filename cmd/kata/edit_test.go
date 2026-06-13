package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// TestEdit_AddsAllFourLinkDirections covers the new add flags on `kata edit`.
// One edit call attaches a parent, a blocks-out, a blocked-by, and a related
// link in a single PATCH; all four must be persisted.
func TestEdit_AddsAllFourLinkDirections(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")
	parent := createIssue(t, env, pid, "the-parent")
	blocked := createIssue(t, env, pid, "the-blocked")
	blocker := createIssue(t, env, pid, "the-blocker")
	peer := createIssue(t, env, pid, "the-peer")

	runCLI(t, env, dir, "edit", subject,
		"--parent", parent,
		"--blocks", blocked,
		"--blocked-by", blocker,
		"--related", peer,
	)

	b := fetchIssueViaHTTP(t, env, pid, subject)
	var sawParent, sawBlocks, sawBlockedBy, sawRelated bool
	for _, l := range b.Links {
		switch l.Type {
		case "parent":
			if l.From.ShortID == subject && l.To.ShortID == parent {
				sawParent = true
			}
		case "blocks":
			switch {
			case l.From.ShortID == subject && l.To.ShortID == blocked:
				sawBlocks = true
			case l.From.ShortID == blocker && l.To.ShortID == subject:
				sawBlockedBy = true
			}
		case "related":
			if (l.From.ShortID == subject && l.To.ShortID == peer) ||
				(l.From.ShortID == peer && l.To.ShortID == subject) {
				sawRelated = true
			}
		}
	}
	assert.True(t, sawParent, "parent link missing")
	assert.True(t, sawBlocks, "blocks link missing")
	assert.True(t, sawBlockedBy, "blocked-by link missing")
	assert.True(t, sawRelated, "related link missing")
}

// TestEdit_RemoveParent_StrictMatch removes the parent link when the asserted
// parent ref matches the current one.
func TestEdit_RemoveParent_StrictMatch(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	child := createIssue(t, env, pid, "child")
	parent := createIssue(t, env, pid, "parent")
	runCLI(t, env, dir, "edit", child, "--parent", parent)

	runCLI(t, env, dir, "edit", child, "--remove-parent", parent)

	b := fetchIssueViaHTTP(t, env, pid, child)
	for _, l := range b.Links {
		assert.NotEqual(t, "parent", l.Type, "parent link should be gone")
	}
}

// TestEdit_RemoveParent_MismatchFails surfaces a 409-flavored error when the
// asserted parent ref does not match the current parent. Protects agents
// from acting on stale state.
func TestEdit_RemoveParent_MismatchFails(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	child := createIssue(t, env, pid, "child")
	parent := createIssue(t, env, pid, "parent")
	wrong := createIssue(t, env, pid, "wrong-parent")
	runCLI(t, env, dir, "edit", child, "--parent", parent)

	_, err := runCLICapture(t, env, dir, "edit", child, "--remove-parent", wrong)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent")
}

// TestEdit_RemoveLinksAreIdempotent succeeds with no error and no panic when
// the requested link is already gone.
func TestEdit_RemoveLinksAreIdempotent(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")
	other := createIssue(t, env, pid, "other")

	runCLI(t, env, dir, "edit", subject, "--remove-blocks", other)

	b := fetchIssueViaHTTP(t, env, pid, subject)
	assert.Empty(t, b.Links, "no links should exist after no-op remove")
}

// TestEdit_LinkFlagsRejectEmptyOrCommaOnly pins that an empty or
// comma-only flag value fails validation before any field landed.
func TestEdit_LinkFlagsRejectEmptyOrCommaOnly(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")

	for _, val := range []string{"", ","} {
		t.Run("blocks="+val, func(t *testing.T) {
			_, err := runCLICapture(t, env, dir, "edit", subject, "--blocks", val)
			require.Error(t, err, "empty/comma-only --blocks must fail validation")
			assert.Contains(t, err.Error(), "must not be empty")
		})
	}

	// Mixed PATCH: --title T --blocks "" must fail entirely; the title
	// update must NOT land.
	_, err := runCLICapture(t, env, dir, "edit", subject, "--title", "NEW", "--blocks", "")
	require.Error(t, err)
	got := fetchIssueViaHTTP(t, env, pid, subject)
	assert.Equal(t, "subject", got.Issue.Title,
		"title must be unchanged when a sibling link flag rejected the whole edit")
}

// TestEdit_LinkFlagsAcceptIssueRefs covers UID forms on link flags.
// Without ref resolution, scripts that link by UID would break silently.
func TestEdit_LinkFlagsAcceptIssueRefs(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")
	parent := createIssue(t, env, pid, "the-parent")

	// Look up the parent's UID so we can pass it as a ref.
	b := fetchIssueViaHTTP(t, env, pid, parent)
	require.NotEmpty(t, b.Issue.UID, "seeded parent must have a UID")

	// Use short_id for URL ref and ULID for --parent.
	runCLI(t, env, dir, "edit", subject, "--parent", b.Issue.UID)

	got := fetchIssueViaHTTP(t, env, pid, subject)
	var sawParent bool
	for _, l := range got.Links {
		if l.Type == "parent" && l.From.ShortID == subject && l.To.ShortID == parent {
			sawParent = true
		}
	}
	assert.True(t, sawParent, "parent link by UID-ref must persist")

	// Remove via short_id.
	runCLI(t, env, dir, "edit", subject, "--remove-parent", parent)
	got = fetchIssueViaHTTP(t, env, pid, subject)
	for _, l := range got.Links {
		assert.NotEqual(t, "parent", l.Type, "parent link must be removable by short_id")
	}
}

// TestEdit_HumanModePrintsLinkSummary verifies the human-mode renderer
// appends a "links: ..." segment listing every applied add/remove.
func TestEdit_HumanModePrintsLinkSummary(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")
	target := createIssue(t, env, pid, "target")

	// Successful add: summary line includes the +blocks segment.
	out := runCLI(t, env, dir, "edit", subject, "--blocks", target)
	assert.Contains(t, out, "+blocks "+target,
		"human-mode edit must report the link mutation: %q", out)

	// Idempotent no-op: same flag again. Daemon returns changed=false
	// with no entries in changes; renderer should print the no-op tail.
	out = runCLI(t, env, dir, "edit", subject, "--blocks", target)
	assert.Contains(t, out, "(no changes applied)",
		"human-mode no-op edit must say so explicitly: %q", out)
}

func TestEdit_AgentOutputIncludesChangedAndChanges(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")
	target := createIssue(t, env, pid, "target")

	out := runCLI(t, env, dir, "--agent", "edit", subject,
		"--title", "renamed subject",
		"--owner", "wesm",
		"--blocks", target)

	assert.Regexp(t, `(?m)^OK edit \S+ changed=true`, out)
	assert.Contains(t, out, "Changes: title, owner, links")
}

func TestEdit_AgentOutputDoesNotReportNoopLinkChanges(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")
	target := createIssue(t, env, pid, "target")

	out := runCLI(t, env, dir, "--agent", "edit", subject,
		"--title", "renamed subject",
		"--remove-blocks", target)

	assert.Regexp(t, `(?m)^OK edit \S+ changed=true`, out)
	assert.Contains(t, out, "Changes: title")
	assert.NotContains(t, out, "Changes: title, links")
}

// TestEdit_DistinctParentRefsRejected covers the at-most-one parent contract.
// --parent A --parent B (or --remove-parent) must error rather than
// silently last-winning so a typo can't mutate the wrong relationship.
func TestEdit_DistinctParentRefsRejected(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")
	p1 := createIssue(t, env, pid, "p1")
	p2 := createIssue(t, env, pid, "p2")

	_, err := runCLICapture(t, env, dir, "edit", subject, "--parent", p1, "--parent", p2)
	require.Error(t, err, "two distinct --parent values must error")
	assert.Contains(t, err.Error(), "--parent")

	_, err = runCLICapture(t, env, dir, "edit", subject, "--remove-parent", p1, "--remove-parent", p2)
	require.Error(t, err, "two distinct --remove-parent values must error")
	assert.Contains(t, err.Error(), "--remove-parent")

	// Repeats of the SAME value are allowed (idempotent for the user).
	runCLI(t, env, dir, "edit", subject, "--parent", p1, "--parent", p1)
}

// TestEdit_EquivalentParentRefsAccepted verifies that at-most-one flags
// accept different ref forms that resolve to the same issue (qualified vs
// bare, short_id vs ULID).
func TestEdit_EquivalentParentRefsAccepted(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")
	parent := createIssue(t, env, pid, "the-parent")

	// Bare and qualified forms must reconcile.
	runCLI(t, env, dir, "edit", subject, "--parent", parent, "--parent", "kata#"+parent)

	got := fetchIssueViaHTTP(t, env, pid, subject)
	var sawParent bool
	for _, l := range got.Links {
		if l.Type == "parent" && l.From.ShortID == subject && l.To.ShortID == parent {
			sawParent = true
		}
	}
	assert.True(t, sawParent, "equivalent --parent refs must reconcile to the same issue")
}

// TestEdit_CrossProjectLinkViaQualifiedRef pins the CLI contract: a
// qualified ref naming another project is forwarded to the daemon and
// the link lands; kata show renders the foreign peer qualified while
// same-project peers stay bare. The edit command's own one-liner output
// must apply the same bare/qualified rule so the user sees the correct
// ref immediately after the mutation.
func TestEdit_CrossProjectLinkViaQualifiedRef(t *testing.T) {
	env := testenv.New(t)
	// hub-project: the workspace the CLI is bound to.
	hubDir := initBoundWorkspace(t, env.URL, "https://github.com/example/hub-project.git")
	hubPID := resolvePIDViaHTTP(t, env.URL, hubDir)
	// spoke-project: a second project the link target lives in.
	spokeDir := initBoundWorkspace(t, env.URL, "https://github.com/example/spoke-project.git")
	spokePID := resolvePIDViaHTTP(t, env.URL, spokeDir)

	subject := createIssue(t, env, hubPID, "subject")
	// same-project peer in hub-project for the bare-rendering assertion.
	sameProjectPeer := createIssue(t, env, hubPID, "same-project-peer")
	// cross-project peer in spoke-project.
	foreignPeer := createIssue(t, env, spokePID, "foreign-peer")

	// Step 1: link subject → spoke-project#foreignPeer (cross-project).
	// The edit command's one-liner must show the foreign peer qualified.
	editOut1 := runCLI(t, env, hubDir, "edit", subject, "--blocks", "spoke-project#"+foreignPeer)
	assert.Contains(t, editOut1, "spoke-project#"+foreignPeer,
		"edit one-liner must render foreign peer qualified:\n%s", editOut1)

	// Step 2: link subject → sameProjectPeer (same project, bare ref).
	// The edit command's one-liner must show the same-project peer bare.
	editOut2 := runCLI(t, env, hubDir, "edit", subject, "--related", sameProjectPeer)
	assert.Contains(t, editOut2, sameProjectPeer,
		"edit one-liner must contain same-project peer:\n%s", editOut2)
	assert.NotContains(t, editOut2, "hub-project#"+sameProjectPeer,
		"edit one-liner must render same-project peer BARE, not qualified:\n%s", editOut2)

	// Step 3: kata show subject must render foreign peer QUALIFIED and
	// same-project peer BARE.
	out := runCLI(t, env, hubDir, "show", subject)
	assert.Contains(t, out, "spoke-project#"+foreignPeer,
		"foreign peer must render qualified in show output:\n%s", out)
	assert.Contains(t, out, sameProjectPeer,
		"same-project peer must appear in show output:\n%s", out)
	assert.NotContains(t, out, "hub-project#"+sameProjectPeer,
		"same-project peer must render BARE, not qualified:\n%s", out)
}

// TestEdit_CrossProjectLinkReversePOV verifies that after a cross-project
// `--blocks` edit, `kata show` on the SPOKE-side issue (from the hub workspace,
// using a qualified ref) renders the hub subject as "blocked-by: hub-project#<sid>".
// This exercises the wire-data reverse-POV rendering path in linkLabelFromPOV:
// from the spoke issue's viewpoint the link type is "blocks" and from == hub
// subject, so linkLabelFromPOV returns ("blocked-by", peerRefForDisplay(from, spokeProject)).
// Since from.Project == "hub-project" != "spoke-project", it must render qualified.
func TestEdit_CrossProjectLinkReversePOV(t *testing.T) {
	env := testenv.New(t)
	hubDir := initBoundWorkspace(t, env.URL, "https://github.com/example/hub-project.git")
	hubPID := resolvePIDViaHTTP(t, env.URL, hubDir)
	spokeDir := initBoundWorkspace(t, env.URL, "https://github.com/example/spoke-project.git")
	spokePID := resolvePIDViaHTTP(t, env.URL, spokeDir)

	subject := createIssue(t, env, hubPID, "hub-subject")
	foreignPeer := createIssue(t, env, spokePID, "spoke-peer")

	// Edit hub subject to blocks spoke-project#foreignPeer.
	runCLI(t, env, hubDir, "edit", subject, "--blocks", "spoke-project#"+foreignPeer)

	// Show the SPOKE issue from the hub workspace via a qualified ref.
	// linkLabelFromPOV for the spoke issue: from.UID == hubSubjectUID, viewer == spokePeer,
	// so the spoke issue is the "to" end → label = "blocked-by", other = hub subject ref.
	spokeShowOut := runCLI(t, env, hubDir, "show", "spoke-project#"+foreignPeer)
	assert.Contains(t, spokeShowOut, "blocked-by: hub-project#"+subject,
		"spoke-side show must render hub subject as 'blocked-by: hub-project#<sid>':\n%s", spokeShowOut)
}

// TestEdit_ConflictDetectedAcrossRefForms pins that the add/remove
// conflict check normalizes refs to canonical UIDs before comparing,
// so spelling the same target two different ways still fires the
// validation error. Without canonicalization, `--blocks abc4
// --remove-blocks <ULID-of-abc4>` would pass string-equality and reach
// the daemon as a contradictory mutation.
func TestEdit_ConflictDetectedAcrossRefForms(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")
	target := createIssueViaHTTPFull(t, env, dir, "target")

	_, err := runCLICapture(t, env, dir, "edit", subject,
		"--blocks", target.ShortID,
		"--remove-blocks", target.UID,
	)
	require.Error(t, err, "same target spelled as short_id and ULID must conflict")
	assert.Contains(t, err.Error(), "--blocks and --remove-blocks both target")
}

// TestEdit_PriorityOnPATCH sets priority via the unified PATCH.
func TestEdit_PriorityOnPATCH(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")

	runCLI(t, env, dir, "edit", subject, "--priority", "2")

	b := fetchIssueViaHTTP(t, env, pid, subject)
	require.NotNil(t, b.Issue.Priority)
	assert.Equal(t, int64(2), *b.Issue.Priority)

	// Clear via the dash sentinel.
	runCLI(t, env, dir, "edit", subject, "--priority", "-")
	b = fetchIssueViaHTTP(t, env, pid, subject)
	assert.Nil(t, b.Issue.Priority)
}

// TestEdit_EmptyTitle_ValidatedClientSide pins that --title "" (or
// whitespace-only) is rejected client-side before reaching the daemon.
func TestEdit_EmptyTitle_ValidatedClientSide(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	subject := createIssue(t, env, pid, "subject")

	for _, blank := range []string{"", "   ", "\t\n"} {
		_, err := runCLICapture(t, env, dir, "edit", subject, "--title", blank)
		ce := requireCLIError(t, err, ExitValidation)
		assert.Equal(t, kindValidation, ce.Kind)
		assert.Contains(t, ce.Message, "must not be empty",
			"validation message should explain the failure for %q", blank)
	}
}

func TestEdit_WithComment_AppendsComment(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "subject")

	runCLI(t, env, dir, "edit", ref, "--priority", "1", "--comment", "bumping for incident")

	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "bumping for incident", got.Comments[0].Body)
	require.NotNil(t, got.Issue.Priority)
	assert.Equal(t, int64(1), *got.Issue.Priority)
}
