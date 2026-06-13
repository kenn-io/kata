package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// moveURL builds the move action URL for the given source project and issue
// ref so the test bodies stay focused on the request shape rather than path
// construction.
func moveURL(env *testenv.Env, fromPID int64, ref string) string {
	return fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", env.URL, fromPID, ref)
}

// seedMovePair sets up two projects (src, tgt) and one issue in src.
func seedMovePair(t *testing.T, env *testenv.Env) (db.Project, db.Project, db.Issue) {
	t.Helper()
	src, err := env.DB.CreateProject(t.Context(), "src")
	require.NoError(t, err)
	tgt, err := env.DB.CreateProject(t.Context(), "tgt")
	require.NoError(t, err)
	iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: src.ID, Title: "Move me", Author: "tester",
	})
	require.NoError(t, err)
	return src, tgt, iss
}

func TestMoveIssue_HappyPath(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)

	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, tgt.UID)
	ifMatch := fmt.Sprintf(`"rev-%d"`, iss.Revision)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	assert.Equal(t, fmt.Sprintf(`"rev-%d"`, iss.Revision+1), resp.Header.Get("ETag"))

	var out struct {
		Issue      db.Issue `json:"issue"`
		EventID    int64    `json:"event_id"`
		NewShortID string   `json:"new_short_id"`
		Changed    bool     `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.True(t, out.Changed)
	assert.Equal(t, tgt.ID, out.Issue.ProjectID)
	assert.Equal(t, out.Issue.ShortID, out.NewShortID)
	assert.NotZero(t, out.EventID)
	assert.Equal(t, iss.Revision+1, out.Issue.Revision)

	// Verify the row really moved.
	stored, err := env.DB.IssueByID(context.Background(), iss.ID)
	require.NoError(t, err)
	assert.Equal(t, tgt.ID, stored.ProjectID)
	assert.Equal(t, out.NewShortID, stored.ShortID)
}

func TestMoveIssue_StaleIfMatch_412(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)

	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, tgt.UID)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, `"rev-99"`)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
}

// TestMoveIssue_RequestShape_400 collapses the four near-identical request
// validation tests (missing actor / to_project_uid / If-Match, bad If-Match
// format). Each subtest builds the request body and headers from a row in
// the table; the expectation is uniformly 400.
func TestMoveIssue_RequestShape_400(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)
	fullBody := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, tgt.UID)
	validIfMatch := fmt.Sprintf(`"rev-%d"`, iss.Revision)

	cases := []struct {
		name    string
		body    string
		ifMatch string
	}{
		{"missing_actor", fmt.Sprintf(`{"to_project_uid":%q}`, tgt.UID), validIfMatch},
		{"missing_to_project_uid", `{"actor":"tester"}`, validIfMatch},
		{"missing_if_match", fullBody, ""},
		{"bad_if_match_format", fullBody, `"banana"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), c.body, c.ifMatch)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// TestMoveIssue_SameProject_400 covers the no-op case where to_project_uid
// resolves to the issue's current project. The handler must reject this with
// a 400 envelope rather than letting the DB layer respond with a generic 500.
func TestMoveIssue_SameProject_400(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, _, iss := seedMovePair(t, env)

	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, src.UID)
	ifMatch := fmt.Sprintf(`"rev-%d"`, iss.Revision)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	bs, _ := io.ReadAll(resp.Body)
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "same_project")
}

func TestMoveIssue_ToProjectUIDNotFound_404(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, _, iss := seedMovePair(t, env)

	// A syntactically valid ULID that doesn't resolve to any project.
	body := `{"actor":"tester","to_project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`
	ifMatch := fmt.Sprintf(`"rev-%d"`, iss.Revision)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestMoveIssue_SourceProjectArchived_404(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)

	// Soft-delete the source project directly via SQL so activeProjectByID rejects it.
	_, err := env.DB.ExecContext(t.Context(),
		`UPDATE projects SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
		src.ID)
	require.NoError(t, err)

	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, tgt.UID)
	ifMatch := fmt.Sprintf(`"rev-%d"`, iss.Revision)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestMoveIssue_TargetProjectArchived_404(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)

	// Archive the target project.
	_, err := env.DB.ExecContext(t.Context(),
		`UPDATE projects SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
		tgt.ID)
	require.NoError(t, err)

	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, tgt.UID)
	ifMatch := fmt.Sprintf(`"rev-%d"`, iss.Revision)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestMoveIssue_IssueNotFound_404(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, _ := seedMovePair(t, env)

	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, tgt.UID)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, "zzzz9"), body, `"rev-1"`)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestMoveIssue_PreservesLinks pins the storage-v16 contract: moving an
// issue never touches its links. Endpoints are row ids and UIDs, both
// stable across a move, so every edge survives verbatim.
func TestMoveIssue_PreservesLinks(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)

	peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: src.ID, Title: "peer stays behind", Author: "tester",
	})
	require.NoError(t, err)
	for _, typ := range []string{"parent", "blocks"} {
		_, err := env.DB.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: iss.ID, ToIssueID: peer.ID, Type: typ, Author: "tester",
		})
		require.NoError(t, err)
	}

	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, tgt.UID)
	ifMatch := fmt.Sprintf(`"rev-%d"`, iss.Revision)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "move must succeed with links anchored; body: %s", raw)

	links, err := env.DB.LinksByIssue(t.Context(), iss.ID)
	require.NoError(t, err)
	require.Len(t, links, 2, "both links survive the move")
	for _, l := range links {
		assert.Equal(t, iss.UID, l.FromIssueUID, "uid endpoints untouched")
		assert.Equal(t, peer.UID, l.ToIssueUID)
	}
}

func TestMoveIssue_RecurrencePinned_409(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)

	// Pin the issue to a recurrence by inserting a recurrences row and
	// pointing issues.recurrence_id at it. This mirrors the DB-layer test
	// helper.
	res, err := env.DB.ExecContext(t.Context(), `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone,
		   template_title, template_body,
		   template_labels, template_metadata, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"REC00000000000000000000999", src.ID, "FREQ=WEEKLY", "2026-05-11", "UTC",
		"tpl", "", `[]`, `{}`, "tester")
	require.NoError(t, err)
	rid, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = env.DB.ExecContext(t.Context(),
		`UPDATE issues SET recurrence_id = ?, occurrence_key = '2026-05-11' WHERE id = ?`,
		rid, iss.ID)
	require.NoError(t, err)

	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, tgt.UID)
	ifMatch := fmt.Sprintf(`"rev-%d"`, iss.Revision)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, iss.ShortID), body, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusConflict, resp.StatusCode, "body: %s", raw)

	var env409 struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env409))
	assert.Equal(t, "recurrence_pinned", env409.Error.Code)
}

// TestEditIssue_CrossProjectLinkTargets pins link-target resolution forms via
// the edit path: a foreign ULID and a foreign qualified ref both resolve
// (links span projects since storage v16), set_parent works cross-project, and
// a nonexistent ULID is a plain issue_not_found with no same-project verbiage
// (misses now mean "not found anywhere", and single-token auth makes that no
// leak).
func TestEditIssue_CrossProjectLinkTargets(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)
	peer1, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: tgt.ID, Title: "peer one", Author: "tester",
	})
	require.NoError(t, err)
	peer2, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: tgt.ID, Title: "peer two", Author: "tester",
	})
	require.NoError(t, err)
	peer3, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: tgt.ID, Title: "peer three", Author: "tester",
	})
	require.NoError(t, err)
	subjectPath := env.URL + issuePathRef(src.ID, iss.ShortID, "")

	// 1. Foreign ULID resolves as an add target.
	body := fmt.Sprintf(`{"actor":"tester","links_delta":{"add_related":[%q]}}`, peer1.UID)
	resp := doPatch(t, env, subjectPath, body, "")
	raw := readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	// 2. Foreign qualified short_id resolves as an add target.
	body = fmt.Sprintf(`{"actor":"tester","links_delta":{"add_blocked_by":[%q]}}`,
		tgt.Name+"#"+peer2.ShortID)
	resp = doPatch(t, env, subjectPath, body, "")
	raw = readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	// 3. Cross-project set_parent via the edit path must work (not only POST
	// /links), and remove_parent must then clean it up.
	qualifiedPeer3 := tgt.Name + "#" + peer3.ShortID
	body = fmt.Sprintf(`{"actor":"tester","links_delta":{"set_parent":%q}}`, qualifiedPeer3)
	resp = doPatch(t, env, subjectPath, body, "")
	raw = readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "set_parent body: %s", raw)

	body = fmt.Sprintf(`{"actor":"tester","links_delta":{"remove_parent":%q}}`, qualifiedPeer3)
	resp = doPatch(t, env, subjectPath, body, "")
	raw = readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "remove_parent body: %s", raw)

	// 4. A nonexistent ULID is a plain issue_not_found with no same-project
	// verbiage.
	bogus := `{"actor":"tester","links_delta":{"add_related":["01ZZZZZZZZZZZZZZZZZZZZZZZZ"]}}`
	resp = doPatch(t, env, subjectPath, bogus, "")
	raw = readClose(t, resp)
	require.Equalf(t, http.StatusNotFound, resp.StatusCode, "body: %s", raw)
	var env404 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env404))
	assert.Equal(t, "issue_not_found", env404.Error.Code)
	assert.Contains(t, env404.Error.Message, "issue not found")
	assert.NotContains(t, env404.Error.Message, "same project")

	// 5. A soft-deleted foreign issue in an active project is resolved with
	// IncludeDeletedNo on the add path, so add_related must 404.
	softDeleted, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: tgt.ID, Title: "soft-deleted foreign peer", Author: "tester",
	})
	require.NoError(t, err)
	_, _, _, err = env.DB.SoftDeleteIssue(t.Context(), softDeleted.ID, "tester")
	require.NoError(t, err)
	body = fmt.Sprintf(`{"actor":"tester","links_delta":{"add_related":[%q]}}`, softDeleted.UID)
	resp = doPatch(t, env, subjectPath, body, "")
	raw = readClose(t, resp)
	require.Equalf(t, http.StatusNotFound, resp.StatusCode, "soft-deleted add body: %s", raw)
}

// TestEditIssue_ArchivedPeerRules pins the archived-peer policy: adds reject a
// target in an archived project with 409 link_target_archived (naming the
// project), while removes still resolve archived peers so existing links can
// always be cleaned up.
func TestEditIssue_ArchivedPeerRules(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)
	peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: tgt.ID, Title: "peer to archive", Author: "tester",
	})
	require.NoError(t, err)
	subjectPath := env.URL + issuePathRef(src.ID, iss.ShortID, "")

	// 1. Link subject→peer while the peer's project is active.
	body := fmt.Sprintf(`{"actor":"tester","links_delta":{"add_related":[%q]}}`, peer.UID)
	resp := doPatch(t, env, subjectPath, body, "")
	raw := readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	// 2. Archive the peer's project.
	_, _, err = env.DB.RemoveProject(t.Context(), db.RemoveProjectParams{
		ProjectID: tgt.ID, Actor: "tester", Force: true,
	})
	require.NoError(t, err)

	// 3. A new add against the archived peer is rejected.
	body = fmt.Sprintf(`{"actor":"tester","links_delta":{"add_blocks":[%q]}}`, peer.UID)
	resp = doPatch(t, env, subjectPath, body, "")
	raw = readClose(t, resp)
	require.Equalf(t, http.StatusConflict, resp.StatusCode, "body: %s", raw)
	var env409 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env409))
	assert.Equal(t, "link_target_archived", env409.Error.Code)
	assert.Contains(t, env409.Error.Message, tgt.Name)
	// Remedy belongs in the hint field, not the message.
	assert.Equal(t, "unarchive the project to add links", env409.Error.Hint)

	// 4. Removing the existing link to the archived peer still resolves.
	body = fmt.Sprintf(`{"actor":"tester","links_delta":{"remove_related":[%q]}}`, peer.UID)
	resp = doPatch(t, env, subjectPath, body, "")
	raw = readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
}

// TestEditIssue_ArchivedPeerRules_QualifiedRef pins the same archived-peer
// policy as TestEditIssue_ArchivedPeerRules but exercises the QUALIFIED-REF
// form ("project#short_id") instead of the bare ULID. This is the form that
// requires ProjectByNameIncludingArchived: the project lookup must succeed
// so the gate can reject adds and permit removes. Swapping that call to the
// active-only ProjectByName would turn the 409 into a 404 and break this test,
// killing Mutation A.
func TestEditIssue_ArchivedPeerRules_QualifiedRef(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)
	peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: tgt.ID, Title: "peer via qualified ref", Author: "tester",
	})
	require.NoError(t, err)
	subjectPath := env.URL + issuePathRef(src.ID, iss.ShortID, "")
	qualifiedRef := tgt.Name + "#" + peer.ShortID

	// 1. Link subject→peer via qualified ref while the peer's project is active.
	body := fmt.Sprintf(`{"actor":"tester","links_delta":{"add_related":[%q]}}`, qualifiedRef)
	resp := doPatch(t, env, subjectPath, body, "")
	raw := readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "initial add body: %s", raw)

	// 2. Archive the peer's project.
	_, _, err = env.DB.RemoveProject(t.Context(), db.RemoveProjectParams{
		ProjectID: tgt.ID, Actor: "tester", Force: true,
	})
	require.NoError(t, err)

	// 3. A new add via qualified ref against the archived peer is rejected with
	// 409 link_target_archived (project resolves via IncludingArchived, then
	// the gate fires). A 404 here would mean the project lookup used the
	// active-only path and lost the archived project entirely.
	body = fmt.Sprintf(`{"actor":"tester","links_delta":{"add_blocks":[%q]}}`, qualifiedRef)
	resp = doPatch(t, env, subjectPath, body, "")
	raw = readClose(t, resp)
	require.Equalf(t, http.StatusConflict, resp.StatusCode, "add body: %s", raw)
	var env409 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env409))
	assert.Equal(t, "link_target_archived", env409.Error.Code)
	assert.Contains(t, env409.Error.Message, tgt.Name)

	// 4. Removing the existing link via qualified ref into the archived project
	// still resolves — tolerant removes must work even when the target project
	// is archived.
	body = fmt.Sprintf(`{"actor":"tester","links_delta":{"remove_related":[%q]}}`, qualifiedRef)
	resp = doPatch(t, env, subjectPath, body, "")
	raw = readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "remove body: %s", raw)
}
