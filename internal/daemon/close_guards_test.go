package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// throttledEventRecord captures the close.throttled events returned by the
// polling endpoint: only the fields the tests care about are decoded.
type throttledEventRecord struct {
	Type         string          `json:"type"`
	Actor        string          `json:"actor"`
	IssueShortID *string         `json:"issue_short_id"`
	Payload      json.RawMessage `json:"payload"`
}

func TestCloseIssue_IdentityModeDoesNotTrustBodySourceTUI(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	pid := mkProject(t, env, "github.com/test/a", "a")
	issue := mkIssue(t, env, pid, "close me")
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost,
		issuePathRef(pid, issue.ShortID, "actions/close"),
		map[string]any{"actor": "someone_else", "source": "tui"},
		map[string]string{"Authorization": "Bearer alice-token"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")

	got, err := env.DB.IssueByID(context.Background(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "open", got.Status)
}

func TestCloseIssue_IdentityModeDBTokenCannotUseTUIBypassForMatchingTokenActor(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	pid := mkProject(t, env, "github.com/test/a", "a")
	issue := mkIssue(t, env, pid, "close me")
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost,
		issuePathRef(pid, issue.ShortID, "actions/close"),
		map[string]any{"actor": "alice", "source": "tui"},
		map[string]string{"Authorization": "Bearer alice-token"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")

	got, err := env.DB.IssueByID(context.Background(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "open", got.Status)
}

// fetchEvents polls /api/v1/events for the project and returns every event in
// chronological (id ASC) order.
func fetchEvents(t *testing.T, env *testenv.Env, projectID int64) []throttledEventRecord {
	t.Helper()
	var body struct {
		Events []throttledEventRecord `json:"events"`
	}
	envGetJSON(t, env,
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/events?after_id=0&limit=1000",
		&body)
	return body.Events
}

// closeIssueWithEvidence posts a close request and returns the response paired
// with the body. Unlike closeIssueAs it does not assert 200, so callers can
// inspect refusals (e.g. 409 parent-has-open-children).
func closeIssueWithEvidence(
	t *testing.T, env *testenv.Env, projectID, issueNumber int64,
	actor, reason, message string, evidence []map[string]any,
) (*http.Response, []byte) {
	t.Helper()
	body := map[string]any{
		"actor":   actor,
		"reason":  reason,
		"message": message,
	}
	if evidence != nil {
		body["evidence"] = evidence
	}
	return envDoRaw(t, env, http.MethodPost,
		issuePath(projectID, issueNumber, "actions/close"), body, nil)
}

func TestParentCloseCompleteness_RefusesWhenOpenChildrenExist(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	child := createIssueViaHTTP(t, env, pid, "first child")
	postLink(t, env, pid, child, "parent", parent)

	resp, bs := closeIssueWithEvidence(t, env, pid, parent, "tester",
		"done",
		"Reviewed parent scope and confirmed all children are completed.",
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	require.Equal(t, http.StatusConflict, resp.StatusCode, string(bs))
	assertAPIError(t, resp.StatusCode, bs, http.StatusConflict, "parent_has_open_children")
	assert.Contains(t, string(bs), "open children")
	assert.Contains(t, string(bs), "first child")
}

func TestParentCloseCompleteness_AllowsWhenChildrenClosed(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent")
	child := createIssueViaHTTP(t, env, pid, "child")
	postLink(t, env, pid, child, "parent", parent)

	// Close the child first with done + evidence.
	childResp, childBody := closeIssueWithEvidence(t, env, pid, child, "tester",
		"done",
		"Implemented the schema review and verified tests pass.",
		[]map[string]any{{"type": "commit", "sha": "def5678"}})
	require.Equalf(t, http.StatusOK, childResp.StatusCode, "child close: %s", string(childBody))

	// Parent close now succeeds because all children are closed.
	parentResp, parentBody := closeIssueWithEvidence(t, env, pid, parent, "tester",
		"done",
		"All children completed; reviewed the integrated outcome thoroughly.",
		[]map[string]any{{"type": "reviewed-paths", "paths": []string{"a.go"}}})
	require.Equalf(t, http.StatusOK, parentResp.StatusCode, "parent close: %s", string(parentBody))
}

func TestParentCloseCompleteness_TruncatesLongChildList(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent with many children")
	// Create more children than the sample limit (10) so the suffix renders.
	const totalChildren = 12
	for i := 0; i < totalChildren; i++ {
		child := createIssueViaHTTP(t, env, pid, "child")
		postLink(t, env, pid, child, "parent", parent)
	}

	resp, bs := closeIssueWithEvidence(t, env, pid, parent, "tester",
		"done",
		"Reviewed every child and confirmed all work is done.",
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	require.Equal(t, http.StatusConflict, resp.StatusCode, string(bs))
	body := string(bs)
	assert.Contains(t, body, "open children")
	// 2 more = total - sample limit (12 - 10).
	assert.Truef(t, strings.Contains(body, "2 more"),
		"expected truncation suffix mentioning '2 more': %s", body)
	// The hint should reference the user-facing parent ref so the
	// suggested command is something the user can actually run.
	parentHint := fmt.Sprintf("kata show %s --json", refForIssue(t, env, parent))
	assert.Truef(t, strings.Contains(body, parentHint),
		"expected suffix hint %q: %s", parentHint, body)
}

func TestCloseDuplicateOf_RejectsMissingTarget(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	issue := createIssueViaHTTP(t, env, pid, "issue")

	// Target #999 does not exist in this project.
	resp, bs := closeIssueWithEvidence(t, env, pid, issue, "tester",
		"duplicate",
		"Same Safari race; merge there.",
		[]map[string]any{{"type": "duplicate-of", "issue_ref": "nope"}})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")
	assert.Contains(t, string(bs), "does not exist")
}

func TestCloseDuplicateOf_RejectsSelfTarget(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	issue := createIssueViaHTTP(t, env, pid, "issue")

	resp, bs := closeIssueWithEvidence(t, env, pid, issue, "tester",
		"duplicate",
		"Same Safari race; merge there.",
		[]map[string]any{{"type": "duplicate-of", "issue_ref": refForIssue(t, env, issue)}})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")
	assert.Contains(t, string(bs), "issue being closed")
}

func TestCloseSupersededBy_RejectsMissingTarget(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	issue := createIssueViaHTTP(t, env, pid, "issue")

	resp, bs := closeIssueWithEvidence(t, env, pid, issue, "tester",
		"superseded",
		"Replaced by the rewrite tracked elsewhere.",
		[]map[string]any{{"type": "superseded-by", "issue_ref": "nope"}})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")
	assert.Contains(t, string(bs), "does not exist")
}

func TestCloseSupersededBy_RejectsSelfTarget(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	issue := createIssueViaHTTP(t, env, pid, "issue")

	resp, bs := closeIssueWithEvidence(t, env, pid, issue, "tester",
		"superseded",
		"Replaced by the rewrite tracked elsewhere.",
		[]map[string]any{{"type": "superseded-by", "issue_ref": refForIssue(t, env, issue)}})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")
	assert.Contains(t, string(bs), "issue being closed")
}

func TestCloseDuplicateOf_AcceptsExistingTarget(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	other := createIssueViaHTTP(t, env, pid, "real target")
	issue := createIssueViaHTTP(t, env, pid, "duplicate of the other")

	resp, bs := closeIssueWithEvidence(t, env, pid, issue, "tester",
		"duplicate",
		"Same Safari race; merge there.",
		[]map[string]any{{"type": "duplicate-of", "issue_ref": refForIssue(t, env, other)}})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "duplicate close: %s", string(bs))
}

func TestSiblingThrottle_FourthCloseUnderSameParentRefused(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	children := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		c := createIssueViaHTTP(t, env, pid, fmt.Sprintf("child %d", i+1))
		postLink(t, env, pid, c, "parent", parent)
		children = append(children, c)
	}

	// First three siblings close successfully with the same actor.
	for i, c := range children[:3] {
		resp, bs := closeIssueWithEvidence(t, env, pid, c, "agent-a",
			"done",
			fmt.Sprintf("Implementation of child %d complete and tested.", i+1),
			[]map[string]any{{"type": "commit", "sha": "abc1234"}})
		require.Equalf(t, http.StatusOK, resp.StatusCode,
			"close child %d: %s", i+1, string(bs))
	}

	// Fourth close by the same actor under the same parent is refused.
	resp, bs := closeIssueWithEvidence(t, env, pid, children[3], "agent-a",
		"done",
		"Implementation of child 4 complete and tested.",
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(bs))
	assertAPIError(t, resp.StatusCode, bs, http.StatusTooManyRequests, "sibling_throttle")
	body := string(bs)
	assert.Contains(t, body, "sibling-close throttle")
	// The error should reference the parent issue short_id.
	assert.Contains(t, body, refForIssue(t, env, parent))
}

// Parent links may span projects, so the sibling cohort can too. The
// throttle counts closes of all children under the shared parent regardless
// of which project each child lives in — distributing sibling closes across
// projects must not reset the counter.
func TestSiblingThrottle_CountsCrossProjectSiblings(t *testing.T) {
	env := testenv.New(t)
	pidA := initWorkspaceViaHTTP(t, env, "https://github.com/example/hub-project.git")
	pidB := mkProject(t, env, "github.com/example/spoke-project", "spoke-project")
	parent := createIssueViaHTTP(t, env, pidA, "parent issue")
	parentIssue, err := env.DB.IssueByID(context.Background(), parent)
	require.NoError(t, err)

	spokeRefs := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		c := createIssueViaHTTP(t, env, pidB, fmt.Sprintf("spoke child %d", i+1))
		spokeRefs = append(spokeRefs, "spoke-project#"+refForIssue(t, env, c))
		resp, _ := postLinkRaw(t, env, pidB, c, map[string]any{
			"actor": "agent-a", "type": "parent", "to_ref": parentIssue.UID})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		closeResp, bs := closeIssueWithEvidence(t, env, pidB, c, "agent-a",
			"done",
			fmt.Sprintf("Implementation of spoke child %d complete and tested.", i+1),
			[]map[string]any{{"type": "commit", "sha": "abc1234"}})
		require.Equalf(t, http.StatusOK, closeResp.StatusCode,
			"close spoke child %d: %s", i+1, string(bs))
	}

	hubChild := createIssueViaHTTP(t, env, pidA, "hub child")
	postLinkAs(t, env, pidA, hubChild, "agent-a", "parent", parent)
	resp, bs := closeIssueWithEvidence(t, env, pidA, hubChild, "agent-a",
		"done",
		"Implementation of hub child complete and tested.",
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	assertAPIError(t, resp.StatusCode, bs, http.StatusTooManyRequests, "sibling_throttle")

	// Short_ids are only unique within a project, so the refusal text and
	// the close.throttled audit payload must qualify foreign cohort
	// members; a bare spoke sid could point a reviewer at the wrong hub
	// issue. The parent shares the refused close's project and stays bare.
	body := string(bs)
	for _, ref := range spokeRefs {
		assert.Contains(t, body, ref,
			"refusal must reference foreign siblings qualified")
	}
	assert.Contains(t, body, refForIssue(t, env, parent))

	events := fetchEvents(t, env, pidA)
	var throttled *throttledEventRecord
	for i := range events {
		if events[i].Type == "close.throttled" {
			throttled = &events[i]
			break
		}
	}
	require.NotNil(t, throttled, "expected a close.throttled event")
	var payload struct {
		Parent string   `json:"parent"`
		Cohort []string `json:"cohort"`
	}
	require.NoError(t, json.Unmarshal(throttled.Payload, &payload),
		"payload: %s", string(throttled.Payload))
	assert.Equal(t, refForIssue(t, env, parent), payload.Parent,
		"same-project parent stays bare")
	assert.ElementsMatch(t, spokeRefs, payload.Cohort,
		"foreign cohort members must be qualified in the audit payload")
}

// Same cross-project cohort rule for the repeated-message guard: an
// identical close message on a sibling in another project is still a
// duplicate.
func TestRepeatedMessageGuard_CountsCrossProjectSiblings(t *testing.T) {
	env := testenv.New(t)
	pidA := initWorkspaceViaHTTP(t, env, "https://github.com/example/hub-project.git")
	pidB := mkProject(t, env, "github.com/example/spoke-project", "spoke-project")
	parent := createIssueViaHTTP(t, env, pidA, "parent issue")
	parentIssue, err := env.DB.IssueByID(context.Background(), parent)
	require.NoError(t, err)

	const message = "Verified fix across the shared component and tests pass."

	spokeChild := createIssueViaHTTP(t, env, pidB, "spoke child")
	resp, _ := postLinkRaw(t, env, pidB, spokeChild, map[string]any{
		"actor": "agent-a", "type": "parent", "to_ref": parentIssue.UID})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	closeResp, bs := closeIssueWithEvidence(t, env, pidB, spokeChild, "agent-a",
		"done", message,
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	require.Equalf(t, http.StatusOK, closeResp.StatusCode, "close spoke child: %s", string(bs))

	hubChild := createIssueViaHTTP(t, env, pidA, "hub child")
	postLinkAs(t, env, pidA, hubChild, "agent-a", "parent", parent)
	closeResp, bs = closeIssueWithEvidence(t, env, pidA, hubChild, "agent-a",
		"done", message,
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	assertAPIError(t, closeResp.StatusCode, bs, http.StatusTooManyRequests, "duplicate_message")

	// The prior close lives in another project: the refusal text and the
	// close.throttled payload must reference it qualified, not by a bare
	// short_id that is only unique within the hub project.
	spokeRef := "spoke-project#" + refForIssue(t, env, spokeChild)
	assert.Contains(t, string(bs), spokeRef,
		"refusal must reference the prior foreign close qualified")

	events := fetchEvents(t, env, pidA)
	var throttled *throttledEventRecord
	for i := range events {
		if events[i].Type == "close.throttled" {
			throttled = &events[i]
			break
		}
	}
	require.NotNil(t, throttled, "expected a close.throttled event")
	var payload struct {
		Parent string  `json:"parent"`
		Prior  *string `json:"prior"`
	}
	require.NoError(t, json.Unmarshal(throttled.Payload, &payload),
		"payload: %s", string(throttled.Payload))
	assert.Equal(t, refForIssue(t, env, parent), payload.Parent,
		"same-project parent stays bare")
	require.NotNil(t, payload.Prior, "duplicate-message path must set prior")
	assert.Equal(t, spokeRef, *payload.Prior,
		"foreign prior must be qualified in the audit payload")
}

func TestSiblingThrottle_AllowsFourthCloseWhenOldestSiblingIsOutsideDefaultWindow(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	children := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		c := createIssueViaHTTP(t, env, pid, fmt.Sprintf("child %d", i+1))
		postLink(t, env, pid, c, "parent", parent)
		children = append(children, c)
	}

	resp, bs := closeIssueWithEvidence(t, env, pid, children[0], "agent-a",
		"done",
		"Implementation of child 1 complete and tested.",
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"close child 1: %s", string(bs))

	oldestOutsideWindow := time.Now().Add(-2 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")
	_, err := env.DB.ExecContext(t.Context(), `
		UPDATE events
		   SET created_at = ?
		 WHERE project_id = ?
		   AND issue_id = ?
		   AND type = 'issue.closed'`,
		oldestOutsideWindow, pid, children[0])
	require.NoError(t, err)

	for i, c := range children[1:3] {
		resp, bs := closeIssueWithEvidence(t, env, pid, c, "agent-a",
			"done",
			fmt.Sprintf("Implementation of child %d complete and tested.", i+2),
			[]map[string]any{{"type": "commit", "sha": "abc1234"}})
		require.Equalf(t, http.StatusOK, resp.StatusCode,
			"close child %d: %s", i+2, string(bs))
	}

	resp, bs = closeIssueWithEvidence(t, env, pid, children[3], "agent-a",
		"done",
		"Implementation of child 4 complete and tested.",
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"oldest sibling close should be outside the default throttle window: %s",
		string(bs))
}

// TestSiblingThrottle_DisabledByConfig pins that operators who opt out
// via [close.throttle] enabled=false get unthrottled sibling closes:
// the same scenario that 429s in the default-config test must succeed
// on every close when the daemon is started with throttle disabled.
func TestSiblingThrottle_DisabledByConfig(t *testing.T) {
	env := testenv.New(t, testenv.WithCloseThrottleDisabled())
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	children := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		c := createIssueViaHTTP(t, env, pid, fmt.Sprintf("child %d", i+1))
		postLink(t, env, pid, c, "parent", parent)
		children = append(children, c)
	}
	for i, c := range children {
		resp, bs := closeIssueWithEvidence(t, env, pid, c, "agent-a",
			"done",
			fmt.Sprintf("Implementation of child %d complete and tested.", i+1),
			[]map[string]any{{"type": "commit", "sha": "abc1234"}})
		require.Equalf(t, http.StatusOK, resp.StatusCode,
			"close child %d with throttle disabled: %s", i+1, string(bs))
	}
}

func TestSiblingThrottle_DifferentActorNotThrottled(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	children := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		c := createIssueViaHTTP(t, env, pid, fmt.Sprintf("child %d", i+1))
		postLink(t, env, pid, c, "parent", parent)
		children = append(children, c)
	}

	// agent-a closes the first three.
	for i, c := range children[:3] {
		resp, bs := closeIssueWithEvidence(t, env, pid, c, "agent-a",
			"done",
			fmt.Sprintf("Implementation of child %d complete and tested.", i+1),
			[]map[string]any{{"type": "commit", "sha": "abc1234"}})
		require.Equalf(t, http.StatusOK, resp.StatusCode,
			"close child %d: %s", i+1, string(bs))
	}

	// A different actor's fourth close is not throttled — the rule is per-actor.
	resp, bs := closeIssueWithEvidence(t, env, pid, children[3], "agent-b",
		"done",
		"Independent review by another actor of child 4 confirms it is done.",
		[]map[string]any{{"type": "commit", "sha": "def5678"}})
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"different actor close should succeed: %s", string(bs))
}

func TestSiblingThrottle_UnparentedIssuesNotThrottled(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	// Four issues with no parent link — the throttle only applies to siblings
	// under a shared parent, so all four closes should succeed.
	issues := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		issues = append(issues, createIssueViaHTTP(t, env, pid,
			fmt.Sprintf("standalone %d", i+1)))
	}
	for i, n := range issues {
		resp, bs := closeIssueWithEvidence(t, env, pid, n, "agent-a",
			"done",
			fmt.Sprintf("Standalone issue %d implementation complete and verified.", i+1),
			[]map[string]any{{"type": "commit", "sha": "abc1234"}})
		require.Equalf(t, http.StatusOK, resp.StatusCode,
			"close standalone %d: %s", i+1, string(bs))
	}
}

func TestRepeatedMessageGuard_RefusesIdenticalSiblingMessage(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	a := createIssueViaHTTP(t, env, pid, "child a")
	b := createIssueViaHTTP(t, env, pid, "child b")
	postLink(t, env, pid, a, "parent", parent)
	postLink(t, env, pid, b, "parent", parent)

	msg := "Schema review complete; table remains metadata-only and unchanged."
	// First close succeeds.
	firstResp, firstBody := closeIssueWithEvidence(t, env, pid, a, "agent-a",
		"audit-no-change", msg,
		[]map[string]any{{"type": "no-change-audit", "rationale": "metadata"}})
	require.Equalf(t, http.StatusOK, firstResp.StatusCode,
		"first close: %s", string(firstBody))

	// Second close with the same message under the same parent by the same
	// actor is refused with 429 duplicate_message.
	resp, bs := closeIssueWithEvidence(t, env, pid, b, "agent-a",
		"audit-no-change", msg,
		[]map[string]any{{"type": "no-change-audit", "rationale": "metadata"}})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(bs))
	assertAPIError(t, resp.StatusCode, bs, http.StatusTooManyRequests, "duplicate_message")
	body := string(bs)
	assert.Contains(t, body, "identical close message")
	// The refusal should reference the prior close's issue short_id.
	assert.Contains(t, body, refForIssue(t, env, a))
}

func TestRepeatedMessageGuard_SkipsForWontfix(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	a := createIssueViaHTTP(t, env, pid, "child a")
	b := createIssueViaHTTP(t, env, pid, "child b")
	postLink(t, env, pid, a, "parent", parent)
	postLink(t, env, pid, b, "parent", parent)

	msg := "Decided not to fix this issue; it falls outside of the current milestone scope."
	// Both wontfix closes with identical messages succeed — the guard only
	// applies to done / audit-no-change reasons.
	firstResp, firstBody := closeIssueWithEvidence(t, env, pid, a, "agent-a",
		"wontfix", msg, nil)
	require.Equalf(t, http.StatusOK, firstResp.StatusCode,
		"first wontfix close: %s", string(firstBody))
	secondResp, secondBody := closeIssueWithEvidence(t, env, pid, b, "agent-a",
		"wontfix", msg, nil)
	require.Equalf(t, http.StatusOK, secondResp.StatusCode,
		"second wontfix close: %s", string(secondBody))
}

func TestRepeatedMessageGuard_SkipsForUnparentedIssues(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	// Two issues with no parent link — the rule applies only to siblings
	// under a shared parent, so identical messages should be allowed.
	a := createIssueViaHTTP(t, env, pid, "standalone a")
	b := createIssueViaHTTP(t, env, pid, "standalone b")

	msg := "Fixed the issue and verified the auth tests pass cleanly across the suite."
	firstResp, firstBody := closeIssueWithEvidence(t, env, pid, a, "agent-a",
		"done", msg,
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	require.Equalf(t, http.StatusOK, firstResp.StatusCode,
		"first close: %s", string(firstBody))
	secondResp, secondBody := closeIssueWithEvidence(t, env, pid, b, "agent-a",
		"done", msg,
		[]map[string]any{{"type": "commit", "sha": "def5678"}})
	require.Equalf(t, http.StatusOK, secondResp.StatusCode,
		"second close: %s", string(secondBody))
}

// TestRepeatedMessageGuard_SkipsSelfAfterReopen pins the
// reopen-then-reclose path: when an issue is closed with message M,
// reopened, and then re-closed with the same message M, the guard
// must NOT match against the issue's OWN prior close. The repeated-
// message rule is intended to catch identical prose across SIBLING
// issues, not to block a legitimate re-close of one issue.
func TestRepeatedMessageGuard_SkipsSelfAfterReopen(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	a := createIssueViaHTTP(t, env, pid, "child a")
	postLink(t, env, pid, a, "parent", parent)

	msg := "Schema review complete; table remains metadata-only and unchanged."
	first, firstBody := closeIssueWithEvidence(t, env, pid, a, "agent-a",
		"audit-no-change", msg,
		[]map[string]any{{"type": "no-change-audit", "rationale": "metadata"}})
	require.Equalf(t, http.StatusOK, first.StatusCode,
		"first close: %s", string(firstBody))

	reopen, reopenBody := envDoRaw(t, env, http.MethodPost,
		issuePath(pid, a, "actions/reopen"),
		map[string]any{"actor": "agent-a"}, nil)
	require.Equalf(t, http.StatusOK, reopen.StatusCode,
		"reopen: %s", string(reopenBody))

	// Re-closing with the same message under the same actor must
	// succeed — the prior close is on THIS issue, not a sibling.
	second, secondBody := closeIssueWithEvidence(t, env, pid, a, "agent-a",
		"audit-no-change", msg,
		[]map[string]any{{"type": "no-change-audit", "rationale": "metadata"}})
	require.Equalf(t, http.StatusOK, second.StatusCode,
		"re-close after reopen must not match against the issue's own prior close: %s",
		string(secondBody))
}

// TestSiblingThrottle_SkipsSelfAfterReopen pins that an issue's own
// prior close does not count toward the burst-throttle quota after
// reopen. Otherwise an actor could exhaust the limit by repeatedly
// reopening one issue.
func TestSiblingThrottle_SkipsSelfAfterReopen(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	children := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		c := createIssueViaHTTP(t, env, pid, fmt.Sprintf("child %d", i+1))
		postLink(t, env, pid, c, "parent", parent)
		children = append(children, c)
	}

	// Close all four children — the fourth would trip the throttle.
	// Close only the first three for now to stay under the limit.
	for i, c := range children[:3] {
		resp, bs := closeIssueWithEvidence(t, env, pid, c, "agent-a",
			"done",
			fmt.Sprintf("Implementation of child %d complete and verified.", i+1),
			[]map[string]any{{"type": "commit", "sha": "abc1234"}})
		require.Equalf(t, http.StatusOK, resp.StatusCode,
			"close child %d: %s", i+1, string(bs))
	}

	// Reopen + re-close children[0] with a distinct message. Without
	// the self-exclude filter, children[0]'s prior close would still
	// count, putting recent siblings at 3 and the re-close at #4 →
	// 429. With the filter it counts at 2 prior siblings (children[1],
	// [2]) and succeeds.
	reopen, reopenBody := envDoRaw(t, env, http.MethodPost,
		issuePath(pid, children[0], "actions/reopen"),
		map[string]any{"actor": "agent-a"}, nil)
	require.Equalf(t, http.StatusOK, reopen.StatusCode,
		"reopen: %s", string(reopenBody))

	resp, bs := closeIssueWithEvidence(t, env, pid, children[0], "agent-a",
		"done",
		"Re-closing after reopen with new prose verifying the fix sticks.",
		[]map[string]any{{"type": "commit", "sha": "def5678"}})
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"re-close after reopen must not count own prior close: %s",
		string(bs))
}

// TestRepeatedMessageGuard_SkipsTUIBypassEmptyMessage pins the
// TUI-consecutive-close path: two TUI closes of sibling issues under
// the same parent within the 30-min window each carry an empty
// message (the TUI bypass stores reason="done", message=""). Without
// the empty-norm short-circuit in CheckRepeatedMessageGuard, the
// second close would match the first's empty message and be refused
// as duplicate_message, breaking the interactive close keystroke.
func TestRepeatedMessageGuard_SkipsTUIBypassEmptyMessage(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	a := createIssueViaHTTP(t, env, pid, "child a")
	b := createIssueViaHTTP(t, env, pid, "child b")
	postLink(t, env, pid, a, "parent", parent)
	postLink(t, env, pid, b, "parent", parent)

	tuiClose := func(issueID int64) (*http.Response, []byte) {
		return envDoRaw(t, env, http.MethodPost,
			issuePath(pid, issueID, "actions/close"),
			map[string]any{"actor": "agent-a", "source": "tui"}, nil)
	}
	firstResp, firstBody := tuiClose(a)
	require.Equalf(t, http.StatusOK, firstResp.StatusCode,
		"first TUI close: %s", string(firstBody))
	secondResp, secondBody := tuiClose(b)
	require.Equalf(t, http.StatusOK, secondResp.StatusCode,
		"second TUI close must not 429 on empty-message match: %s",
		string(secondBody))
}

func TestThrottle_EmitsCloseThrottledEvent_SiblingBurst(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	children := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		c := createIssueViaHTTP(t, env, pid, fmt.Sprintf("child %d", i+1))
		postLink(t, env, pid, c, "parent", parent)
		children = append(children, c)
	}

	// First three siblings close successfully.
	for i, c := range children[:3] {
		resp, bs := closeIssueWithEvidence(t, env, pid, c, "agent-a",
			"done",
			fmt.Sprintf("Implementation of child %d complete and tested.", i+1),
			[]map[string]any{{"type": "commit", "sha": "abc1234"}})
		require.Equalf(t, http.StatusOK, resp.StatusCode,
			"close child %d: %s", i+1, string(bs))
	}

	// Fourth close is throttled.
	resp, bs := closeIssueWithEvidence(t, env, pid, children[3], "agent-a",
		"done",
		"Implementation of child 4 complete and tested.",
		[]map[string]any{{"type": "commit", "sha": "abc1234"}})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(bs))

	// The throttle should have emitted a close.throttled event for the
	// refused issue (children[3]) with reason=sibling-burst, parent ==
	// the parent's user-facing number, and the recent-sibling cohort.
	events := fetchEvents(t, env, pid)
	var throttled *throttledEventRecord
	for i := range events {
		if events[i].Type == "close.throttled" {
			throttled = &events[i]
			break
		}
	}
	require.NotNil(t, throttled, "expected one close.throttled event")
	assert.Equal(t, "agent-a", throttled.Actor)
	require.NotNil(t, throttled.IssueShortID)
	assert.Equal(t, refForIssue(t, env, children[3]), *throttled.IssueShortID,
		"event should be attached to the refused issue")

	var payload struct {
		Reason string   `json:"reason"`
		Parent string   `json:"parent"`
		Cohort []string `json:"cohort"`
		Prior  *string  `json:"prior,omitempty"`
	}
	require.NoError(t, json.Unmarshal(throttled.Payload, &payload),
		"payload: %s", string(throttled.Payload))
	assert.Equal(t, "sibling-burst", payload.Reason)
	assert.Equal(t, refForIssue(t, env, parent), payload.Parent)
	assert.Nil(t, payload.Prior, "sibling-burst path should not set prior")
	// cohort = the three closes that triggered the throttle, in some order.
	wantCohort := make([]string, 0, 3)
	for _, c := range children[:3] {
		wantCohort = append(wantCohort, refForIssue(t, env, c))
	}
	assert.ElementsMatch(t, wantCohort, payload.Cohort,
		"cohort should list the recent sibling closes")
}

func TestThrottle_EmitsCloseThrottledEvent_DuplicateMessage(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	a := createIssueViaHTTP(t, env, pid, "child a")
	b := createIssueViaHTTP(t, env, pid, "child b")
	postLink(t, env, pid, a, "parent", parent)
	postLink(t, env, pid, b, "parent", parent)

	msg := "Schema review complete; table remains metadata-only and unchanged."
	firstResp, firstBody := closeIssueWithEvidence(t, env, pid, a, "agent-a",
		"audit-no-change", msg,
		[]map[string]any{{"type": "no-change-audit", "rationale": "metadata"}})
	require.Equalf(t, http.StatusOK, firstResp.StatusCode,
		"first close: %s", string(firstBody))

	resp, bs := closeIssueWithEvidence(t, env, pid, b, "agent-a",
		"audit-no-change", msg,
		[]map[string]any{{"type": "no-change-audit", "rationale": "metadata"}})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(bs))

	events := fetchEvents(t, env, pid)
	var throttled *throttledEventRecord
	for i := range events {
		if events[i].Type == "close.throttled" {
			throttled = &events[i]
			break
		}
	}
	require.NotNil(t, throttled, "expected one close.throttled event")
	require.NotNil(t, throttled.IssueShortID)
	assert.Equal(t, refForIssue(t, env, b), *throttled.IssueShortID)

	var payload struct {
		Reason string   `json:"reason"`
		Parent string   `json:"parent"`
		Cohort []string `json:"cohort"`
		Prior  *string  `json:"prior,omitempty"`
	}
	require.NoError(t, json.Unmarshal(throttled.Payload, &payload),
		"payload: %s", string(throttled.Payload))
	assert.Equal(t, "duplicate-message", payload.Reason)
	assert.Equal(t, refForIssue(t, env, parent), payload.Parent)
	assert.Empty(t, payload.Cohort,
		"duplicate-message path should not set cohort")
	require.NotNil(t, payload.Prior,
		"duplicate-message path should set prior to the matching close")
	assert.Equal(t, refForIssue(t, env, a), *payload.Prior)
}

func TestThrottle_DryRunDoesNotEmitEvent(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent issue")
	children := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		c := createIssueViaHTTP(t, env, pid, fmt.Sprintf("child %d", i+1))
		postLink(t, env, pid, c, "parent", parent)
		children = append(children, c)
	}
	for i, c := range children[:3] {
		resp, bs := closeIssueWithEvidence(t, env, pid, c, "agent-a",
			"done",
			fmt.Sprintf("Implementation of child %d complete and tested.", i+1),
			[]map[string]any{{"type": "commit", "sha": "abc1234"}})
		require.Equalf(t, http.StatusOK, resp.StatusCode,
			"close child %d: %s", i+1, string(bs))
	}

	resp, bs := envDoRaw(t, env, http.MethodPost,
		issuePath(pid, children[3], "actions/close"),
		map[string]any{
			"actor":   "agent-a",
			"reason":  "done",
			"message": "Implementation of child 4 complete and tested.",
			"evidence": []map[string]any{
				{"type": "commit", "sha": "abc1234"},
			},
			"dry_run": true,
		}, nil)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(bs))

	for _, ev := range fetchEvents(t, env, pid) {
		assert.NotEqualf(t, "close.throttled", ev.Type,
			"dry-run throttle must not emit close.throttled (got payload %s)",
			string(ev.Payload))
	}
}
