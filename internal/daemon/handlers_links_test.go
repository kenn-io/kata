package daemon_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCreateLink_HappyPath(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env)
	from, err := env.DB.IssueByID(t.Context(), a)
	require.NoError(t, err)
	to, err := env.DB.IssueByID(t.Context(), b)
	require.NoError(t, err)

	out := postLink(t, env, pid, a, "blocks", b)
	assert.Equal(t, "blocks", out.Link.Type)
	assert.Equal(t, from.ShortID, out.Link.From.ShortID)
	assert.Equal(t, to.ShortID, out.Link.To.ShortID)
	project, err := env.DB.ProjectByID(t.Context(), pid)
	require.NoError(t, err)
	assert.Equal(t, from.UID, out.Link.From.UID)
	assert.Equal(t, to.UID, out.Link.To.UID)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.linked", out.Event.Type)
	assert.Equal(t, project.UID, out.Event.ProjectUID)
	require.NotNil(t, out.Event.IssueUID)
	require.NotNil(t, out.Event.RelatedIssueUID)
	assert.Equal(t, from.UID, *out.Event.IssueUID)
	assert.Equal(t, to.UID, *out.Event.RelatedIssueUID)
	assert.True(t, out.Changed)
}

func TestCreateLink_DuplicateIsNoop(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env)
	postLink(t, env, pid, a, "blocks", b)

	out := postLink(t, env, pid, a, "blocks", b)
	assert.Nil(t, out.Event, "duplicate link is no-op (event:null)")
	assert.False(t, out.Changed)
}

func TestCreateLink_RelatedCanonicalizesOrder(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env) // a < b
	aIss, err := env.DB.IssueByID(t.Context(), a)
	require.NoError(t, err)
	bIss, err := env.DB.IssueByID(t.Context(), b)
	require.NoError(t, err)
	out := postLink(t, env, pid, b, "related", a) // user passes b → a
	assert.Equal(t, "related", out.Link.Type)
	assert.Equal(t, aIss.ShortID, out.Link.From.ShortID, "canonical: from is lower-numbered side")
	assert.Equal(t, bIss.ShortID, out.Link.To.ShortID)
}

// TestCreateLink_RelatedEventAttributionIsURLIssue verifies that when a user
// POSTs a `related` link from the higher-numbered side and the handler
// canonicalizes storage to (from < to), the resulting event still attributes
// to the URL's issue (not the canonical-from). The link row records the
// canonical relationship; the event records the user's action.
func TestCreateLink_RelatedEventAttributionIsURLIssue(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env) // a < b
	aIss, err := env.DB.IssueByID(t.Context(), a)
	require.NoError(t, err)
	bIss, err := env.DB.IssueByID(t.Context(), b)
	require.NoError(t, err)
	// POST from b (higher-numbered) targeting a. Storage canonicalizes
	// to (a→b), but the event must still be attributed to issue b.
	out := postLink(t, env, pid, b, "related", a)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.linked", out.Event.Type)

	// The response carries the canonical link (from=a, to=b).
	assert.Equal(t, aIss.ShortID, out.Link.From.ShortID)
	assert.Equal(t, bIss.ShortID, out.Link.To.ShortID)

	// Query the events table directly: events.issue_uid must be b's UID (URL),
	// and the payload's from_short_id / to_short_id must record what the
	// user did (from b → to a), not the canonical link's columns.
	row := env.DB.QueryRowContext(t.Context(),
		`SELECT issue_uid, payload FROM events
		 WHERE project_id = ? AND type = 'issue.linked'
		 ORDER BY id DESC LIMIT 1`, pid)
	var issueUID, payload string
	require.NoError(t, row.Scan(&issueUID, &payload))
	assert.Equal(t, bIss.UID, issueUID, "event must attribute to URL issue (b), not canonical-from (a)")

	var pl struct {
		FromShortID string `json:"from_short_id"`
		ToShortID   string `json:"to_short_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &pl))
	assert.Equal(t, bIss.ShortID, pl.FromShortID, "payload from_short_id is the URL issue's short_id")
	assert.Equal(t, aIss.ShortID, pl.ToShortID, "payload to_short_id is the OTHER endpoint")
}

func TestCreateLink_ParentAlreadySetIs409(t *testing.T) {
	env := testenv.New(t)
	pid, child, p1 := setupTwoIssues(t, env)
	p2 := createIssueViaHTTP(t, env, pid, "p2")
	postLink(t, env, pid, child, "parent", p1)

	p2Ref := refForIssue(t, env, p2)
	resp, _ := postLinkRaw(t, env, pid, child, map[string]any{
		"actor":  "tester",
		"type":   "parent",
		"to_ref": p2Ref,
	})
	assert.Equal(t, 409, resp.StatusCode)
}

func TestCreateLink_ParentReplaceSwapsParent(t *testing.T) {
	env := testenv.New(t)
	pid, child, p1 := setupTwoIssues(t, env)
	p2 := createIssueViaHTTP(t, env, pid, "p2")
	postLink(t, env, pid, child, "parent", p1)

	p2Iss, err := env.DB.IssueByID(t.Context(), p2)
	require.NoError(t, err)
	resp, out := postLinkRaw(t, env, pid, child, map[string]any{
		"actor":   "tester",
		"type":    "parent",
		"to_ref":  p2Iss.ShortID,
		"replace": true,
	})
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, p2Iss.ShortID, out.Link.To.ShortID)
}

func TestCreateLink_ParentReplaceUnlinkEventPointsToOldParent(t *testing.T) {
	env := testenv.New(t)
	pid, child, p1 := setupTwoIssues(t, env)
	p2 := createIssueViaHTTP(t, env, pid, "p2")
	postLink(t, env, pid, child, "parent", p1)

	p2Ref := refForIssue(t, env, p2)
	resp, _ := postLinkRaw(t, env, pid, child, map[string]any{
		"actor":   "tester",
		"type":    "parent",
		"to_ref":  p2Ref,
		"replace": true,
	})
	require.Equal(t, 200, resp.StatusCode)

	// The unlink event isn't in the response (response carries only the
	// linked event). Query the events table directly to verify the unlink
	// event references the OLD parent (p1), not the new (p2).
	p1Iss, err := env.DB.IssueByID(t.Context(), p1)
	require.NoError(t, err)
	var pl struct {
		ToShortID string `json:"to_short_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(lastEventPayload(t, env, pid, "issue.unlinked")), &pl))
	assert.Equal(t, p1Iss.ShortID, pl.ToShortID, "unlink event must reference the old parent's short_id")
}

// TestCreateLink_ParentReplaceSelfLinkLeavesNoMutation verifies that a
// self-link rejected on the parent --replace path returns 400 BEFORE deleting
// the existing parent. With the bug, DeleteLinkAndEvent would have committed
// the unlink (row + event) before CreateLinkAndEvent surfaced ErrSelfLink. We
// assert directly against the events and links tables: no issue.unlinked
// event exists, and the original parent link is still attached.
func TestCreateLink_ParentReplaceSelfLinkLeavesNoMutation(t *testing.T) {
	env := testenv.New(t)
	pid, child, p1 := setupTwoIssues(t, env)
	postLink(t, env, pid, child, "parent", p1)

	childRef := refForIssue(t, env, child)
	resp, _ := postLinkRaw(t, env, pid, child, map[string]any{
		"actor":   "tester",
		"type":    "parent",
		"to_ref":  childRef,
		"replace": true,
	})
	require.Equal(t, 400, resp.StatusCode, "self-link must be rejected before mutation")

	// No issue.unlinked event was inserted. The bug's signature was a
	// committed unlink event followed by a 400; the fix's signature is
	// zero unlink events.
	var unlinkedCount int
	require.NoError(t, env.DB.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM events WHERE project_id = ? AND type = 'issue.unlinked'`,
		pid).Scan(&unlinkedCount))
	assert.Equal(t, 0, unlinkedCount, "no issue.unlinked event should exist after rejected self-link")

	// And the original parent link row itself is still attached.
	var parentLinks int
	require.NoError(t, env.DB.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM links
		   WHERE from_issue_id IN (SELECT id FROM issues WHERE project_id = ?)
		     AND type = 'parent'`,
		pid).Scan(&parentLinks))
	assert.Equal(t, 1, parentLinks, "original parent link must still exist")
}

// TestCreateLink_ParentReplaceCycleLeavesNoMutation verifies that a parent
// --replace whose new parent would close a cycle returns 400 BEFORE deleting
// the existing parent. Chain: c's parent = p1, d's parent = c (so d is a
// descendant of c). Replacing c's parent with d would create c -> d -> c.
// With the bug, DeleteLinkAndEvent committed the unlink (row + event) before
// the in-tx cycle guard fired, leaving c parentless. We assert directly
// against the events and links tables: no NEW issue.unlinked event for c, and
// c's parent is still p1.
func TestCreateLink_ParentReplaceCycleLeavesNoMutation(t *testing.T) {
	env := testenv.New(t)
	pid, c, p1 := setupTwoIssues(t, env)
	d := createIssueViaHTTP(t, env, pid, "d")
	cIss, err := env.DB.IssueByID(t.Context(), c)
	require.NoError(t, err)
	p1Iss, err := env.DB.IssueByID(t.Context(), p1)
	require.NoError(t, err)
	dIss, err := env.DB.IssueByID(t.Context(), d)
	require.NoError(t, err)

	postLink(t, env, pid, c, "parent", p1) // c's parent = p1
	postLink(t, env, pid, d, "parent", c)  // d's parent = c (d descends from c)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		issuePathRef(pid, cIss.ShortID, "links"), map[string]any{
			"actor":   "tester",
			"type":    "parent",
			"to_ref":  dIss.ShortID,
			"replace": true,
		}, nil)
	require.Equalf(t, http.StatusBadRequest, resp.StatusCode,
		"a cycle-rejected replace must 400, body: %s", raw)
	var env400 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env400))
	assert.Equal(t, "validation", env400.Error.Code)
	assert.Equal(t, "set_parent on #"+cIss.ShortID+" would create a parent cycle",
		env400.Error.Message)

	// No issue.unlinked event for c: the bug's signature was a committed unlink
	// (old parent p1 removed) followed by the 400.
	var unlinkedForC int
	require.NoError(t, env.DB.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM events WHERE type = 'issue.unlinked' AND issue_id = ?`,
		c).Scan(&unlinkedForC))
	assert.Equal(t, 0, unlinkedForC, "no issue.unlinked event should exist for c after rejected replace")

	// c's parent is still p1.
	parent, err := env.DB.ParentOf(t.Context(), c)
	require.NoError(t, err, "c must still have a parent")
	assert.Equal(t, p1Iss.ID, parent.ToIssueID, "c's parent must still be p1 after a rejected replace")
}

func TestCreateLink_BlankActorIs400(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env)
	bRef := refForIssue(t, env, b)
	resp, _ := postLinkRaw(t, env, pid, a, map[string]any{
		"actor":  "   ",
		"type":   "blocks",
		"to_ref": bRef,
	})
	assert.Equal(t, 400, resp.StatusCode)
}

func TestDeleteLink_BlankActorIs400(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env)
	created := postLink(t, env, pid, a, "blocks", b)
	resp, _ := deleteLinkAs(t, env, pid, a, "  ", created.Link.ID)
	assert.Equal(t, 400, resp.StatusCode)
}

func TestCreateLink_SelfLinkIs400(t *testing.T) {
	env := testenv.New(t)
	pid, a, _ := setupTwoIssues(t, env)
	aRef := refForIssue(t, env, a)
	resp, _ := postLinkRaw(t, env, pid, a, map[string]any{
		"actor":  "tester",
		"type":   "blocks",
		"to_ref": aRef,
	})
	assert.Equal(t, 400, resp.StatusCode)
}

func TestDeleteLink_RemovesAndEmitsUnlink(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env)
	created := postLink(t, env, pid, a, "blocks", b)

	resp, out := deleteLink(t, env, pid, a, created.Link.ID)
	require.Equal(t, 200, resp.StatusCode)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.unlinked", out.Event.Type)
	assert.True(t, out.Changed)
}

// TestDeleteLink_NotAttachedToURLIssueIs404 verifies that a DELETE on
// /issues/{c}/links/{link_id} where the link is between (a, b) — neither of
// which is c — returns 404 instead of mutating the wrong issue's link and
// emitting a misattributed unlink event.
func TestDeleteLink_NotAttachedToURLIssueIs404(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env)
	c := createIssueViaHTTP(t, env, pid, "c")
	created := postLink(t, env, pid, a, "blocks", b)

	resp, _ := deleteLink(t, env, pid, c, created.Link.ID)
	assert.Equal(t, 404, resp.StatusCode)
}

func TestDeleteLink_AbsentIs200NoOp(t *testing.T) {
	env := testenv.New(t)
	pid, a, _ := setupTwoIssues(t, env)
	resp, out := deleteLink(t, env, pid, a, 9999)
	require.Equal(t, 200, resp.StatusCode)
	assert.Nil(t, out.Event)
	assert.False(t, out.Changed)
}

// TestDeleteLink_EventPayloadOrientsFromURLIssue pins the unlink payload
// orientation: the URL issue (the one whose /links/{id} was DELETEd) is
// always in from_*; the peer is in to_*. This matches createLink's
// attribution (events always tell "the URL issue did X to its peer"),
// regardless of which side the stored link row's columns hold.
func TestDeleteLink_EventPayloadOrientsFromURLIssue(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env)
	aIss, err := env.DB.IssueByID(t.Context(), a)
	require.NoError(t, err)
	bIss, err := env.DB.IssueByID(t.Context(), b)
	require.NoError(t, err)

	// Create link A→B (blocks). Stored as from=A, to=B in the link row.
	created := postLink(t, env, pid, a, "blocks", b)

	// DELETE from B's URL. Even though the stored link has from=A, the
	// event payload must orient from the URL issue (B), so:
	//   from_short_id == B (URL)
	//   to_short_id   == A (peer)
	resp, _ := deleteLink(t, env, pid, b, created.Link.ID)
	require.Equal(t, 200, resp.StatusCode)

	var pl struct {
		FromShortID string `json:"from_short_id"`
		ToShortID   string `json:"to_short_id"`
		FromUID     string `json:"from_uid"`
		ToUID       string `json:"to_uid"`
	}
	require.NoError(t, json.Unmarshal([]byte(lastEventPayload(t, env, pid, "issue.unlinked")), &pl))
	assert.Equal(t, bIss.ShortID, pl.FromShortID, "from_short_id must be the URL issue (B)")
	assert.Equal(t, aIss.ShortID, pl.ToShortID, "to_short_id must be the peer (A)")
	assert.Equal(t, bIss.UID, pl.FromUID, "from_uid must be the URL issue (B)")
	assert.Equal(t, aIss.UID, pl.ToUID, "to_uid must be the peer (A)")
}

// TestCreateLink_ArchivedPeerIs409 pins that POST /links rejects a link whose
// target lives in an archived project with 409 link_target_archived.
// requireLinkTargetAddable is wired in the POST handler (handlers_links.go);
// this test ensures that path is covered alongside the edit and create-issue
// paths that already have archived-target tests.
func TestCreateLink_ArchivedPeerIs409(t *testing.T) {
	env := testenv.New(t)
	src, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	tgt, err := env.DB.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	subject, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: src.ID, Title: "subject", Author: "tester",
	})
	require.NoError(t, err)
	peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: tgt.ID, Title: "peer", Author: "tester",
	})
	require.NoError(t, err)

	// Archive the peer's project before attempting to link.
	_, _, err = env.DB.RemoveProject(t.Context(), db.RemoveProjectParams{
		ProjectID: tgt.ID, Actor: "tester", Force: true,
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		issuePathRef(src.ID, subject.ShortID, "links"),
		map[string]any{"actor": "tester", "type": "blocks", "to_ref": peer.UID},
		nil)
	require.Equalf(t, http.StatusConflict, resp.StatusCode, "body: %s", raw)
	var env409 struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env409))
	assert.Equal(t, "link_target_archived", env409.Error.Code)
}

// TestCreateLink_SameProjectParentCycleParity pins that POST /links parent
// creation runs the same in-tx cycle check as the edit path and emits the
// byte-identical wire error. b's parent is a; POSTing a --parent--> b would
// close the loop and must be rejected with the edit path's exact code and
// message (a is the child whose parent is being set).
func TestCreateLink_SameProjectParentCycleParity(t *testing.T) {
	env := testenv.New(t)
	pid, a, b := setupTwoIssues(t, env)
	aIss, err := env.DB.IssueByID(t.Context(), a)
	require.NoError(t, err)
	bIss, err := env.DB.IssueByID(t.Context(), b)
	require.NoError(t, err)
	// b's parent = a, so a is an ancestor of b.
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{
		FromIssueID: b, ToIssueID: a, Type: "parent", Author: "tester",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		issuePathRef(pid, aIss.ShortID, "links"), map[string]string{
			"actor":  "tester",
			"type":   "parent",
			"to_ref": bIss.ShortID,
		}, nil)
	require.Equalf(t, http.StatusBadRequest, resp.StatusCode, "body: %s", raw)
	var env400 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env400))
	assert.Equal(t, "validation", env400.Error.Code)
	assert.Equal(t, "set_parent on #"+aIss.ShortID+" would create a parent cycle", env400.Error.Message)
}

// TestCreateLink_ParentCycleParity pins that POST /links parent creation runs
// the same in-tx cycle check as the edit path for a chain that spans projects:
// a in src has parent b in tgt; POSTing b --parent--> a closes the loop and
// must be rejected with the byte-identical cycle error the edit path emits.
func TestCreateLink_ParentCycleParity(t *testing.T) {
	env := testenv.New(t)
	src, err := env.DB.CreateProject(t.Context(), "src")
	require.NoError(t, err)
	tgt, err := env.DB.CreateProject(t.Context(), "tgt")
	require.NoError(t, err)
	a, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: src.ID, Title: "a", Author: "tester",
	})
	require.NoError(t, err)
	b, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: tgt.ID, Title: "b", Author: "tester",
	})
	require.NoError(t, err)
	// a's parent = b (cross-project), so b is an ancestor of a.
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{
		FromIssueID: a.ID, ToIssueID: b.ID, Type: "parent", Author: "tester",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		issuePathRef(tgt.ID, b.ShortID, "links"), map[string]string{
			"actor":  "tester",
			"type":   "parent",
			"to_ref": a.UID,
		}, nil)
	require.Equalf(t, http.StatusBadRequest, resp.StatusCode, "body: %s", raw)
	var env400 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env400))
	assert.Equal(t, "validation", env400.Error.Code)
	assert.Equal(t, "set_parent on #"+b.ShortID+" would create a parent cycle", env400.Error.Message)
}
