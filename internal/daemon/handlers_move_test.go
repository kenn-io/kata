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

func TestMoveIssue_CrossProjectLinks_409_WithBlockers(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, a := seedMovePair(t, env)
	b, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: src.ID, Title: "B", Author: "tester",
	})
	require.NoError(t, err)

	// Create a blocks link between a and b inside src — moving a would
	// produce a cross-project link with b.
	_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{
		ProjectID:   src.ID,
		FromIssueID: a.ID,
		ToIssueID:   b.ID,
		Type:        "blocks",
		Author:      "tester",
	})
	require.NoError(t, err)

	body := fmt.Sprintf(`{"actor":"tester","to_project_uid":%q}`, tgt.UID)
	ifMatch := fmt.Sprintf(`"rev-%d"`, a.Revision)
	resp := doPostWithIfMatch(t, env, moveURL(env, src.ID, a.ShortID), body, ifMatch)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusConflict, resp.StatusCode, "body: %s", raw)

	var env409 struct {
		Status int `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Blockers []struct {
					LinkID  int64  `json:"link_id"`
					PeerUID string `json:"peer_uid"`
					Type    string `json:"type"`
				} `json:"blockers"`
			} `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env409))
	assert.Equal(t, "cross_project_links", env409.Error.Code)
	assert.Contains(t, env409.Error.Message, "would become cross-project",
		"message must explain the links are not yet cross-project; the move would make them so")
	require.Len(t, env409.Error.Data.Blockers, 1)
	assert.Equal(t, "blocks", env409.Error.Data.Blockers[0].Type)
	assert.Equal(t, b.UID, env409.Error.Data.Blockers[0].PeerUID)
	assert.NotZero(t, env409.Error.Data.Blockers[0].LinkID)
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

// TestEditIssue_LinkTargetULIDInOtherProject_404 pins the daemon-side scoping
// of link refs: a links_delta add naming the ULID of an issue that lives in a
// different project resolves to issue_not_found. Cross-project links are not
// supported (the schema forbids the rows), and ULID resolution for link refs
// is deliberately scoped to the URL issue's project so the endpoint cannot be
// used to fish for foreign issues. The 404 message must spell out the
// same-project constraint instead of a bare "issue not found" — the miss is
// otherwise indistinguishable from a typo'd ref, and the response must read
// the same whether or not the foreign issue exists.
func TestEditIssue_LinkTargetULIDInOtherProject_404(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	src, tgt, iss := seedMovePair(t, env)
	foreign, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: tgt.ID, Title: "peer in another project", Author: "tester",
	})
	require.NoError(t, err)

	body := fmt.Sprintf(`{"actor":"tester","links_delta":{"add_related":[%q]}}`, foreign.UID)
	resp := doPatch(t, env, env.URL+issuePathRef(src.ID, iss.ShortID, ""), body, "")
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusNotFound, resp.StatusCode, "body: %s", raw)

	var env404 struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env404))
	assert.Equal(t, "issue_not_found", env404.Error.Code)
	assert.Contains(t, env404.Error.Message, "links can only join issues in the same project")

	// A nonexistent ULID must produce the identical message so the
	// response does not leak whether a foreign issue exists.
	bogus := `{"actor":"tester","links_delta":{"add_related":["01ZZZZZZZZZZZZZZZZZZZZZZZZ"]}}`
	resp2 := doPatch(t, env, env.URL+issuePathRef(src.ID, iss.ShortID, ""), bogus, "")
	defer func() { _ = resp2.Body.Close() }()
	raw2, _ := io.ReadAll(resp2.Body)
	require.Equalf(t, http.StatusNotFound, resp2.StatusCode, "body: %s", raw2)
	var env404b struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw2, &env404b))
	assert.Equal(t, env404.Error.Message, env404b.Error.Message,
		"404 message must be identical for foreign and nonexistent link targets")
}
