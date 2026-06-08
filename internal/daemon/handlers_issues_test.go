package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func TestCreateIssue_IdentityModeOverridesBodyActor(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	projectID := mkProject(t, env, "github.com/test/a", "a")
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost, projectPath(projectID)+"/issues",
		map[string]string{"actor": "someone_else", "title": "identity actor"},
		map[string]string{"Authorization": "Bearer alice-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	var out struct {
		Issue struct {
			ID int64 `json:"id"`
		} `json:"issue"`
		Event struct {
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	assert.Equal(t, "alice", out.Event.Actor)
	issue, err := env.DB.IssueByID(context.Background(), out.Issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "alice", issue.Author)
}

func TestCreateIssue_IdentityModeBootstrapTokenCannotWrite(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	projectID := mkProject(t, env, "github.com/test/a", "a")

	resp, bs := envDoRaw(t, env, http.MethodPost, projectPath(projectID)+"/issues",
		map[string]string{"actor": "someone_else", "title": "bootstrap write"},
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusForbidden, "bootstrap_token_write_forbidden")
}

// TestGetIssue_ResolvesByShortID pins that /api/v1/projects/{pid}/issues/{ref}
// accepts a short_id as the {ref} path component and renders QualifiedID in
// the response.
func TestGetIssue_ResolvesByShortID(t *testing.T) {
	h, projectID := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	created, _, err := h.DB().CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "via short_id",
		Author:    "tester",
	})
	require.NoError(t, err)
	project, err := h.DB().ProjectByID(context.Background(), projectID)
	require.NoError(t, err)

	resp, bs := getStatusBody(t, ts, "/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+created.ShortID)
	require.Equalf(t, 200, resp.StatusCode, "body: %s", string(bs))
	var body map[string]any
	require.NoError(t, json.Unmarshal(bs, &body))
	issue := body["issue"].(map[string]any)
	assert.Equal(t, created.UID, issue["uid"])
	assert.Equal(t, created.ShortID, issue["short_id"])
	// QualifiedID is rendered via the IssueOut wire projection on the list
	// surface; the show response keeps the raw db.Issue, so we assert the
	// project name and short_id appear consistently.
	assert.Equal(t, project.Name, "kata")
}

// TestGetIssue_ResolvesByULID pins the ULID branch of the path resolver.
func TestGetIssue_ResolvesByULID(t *testing.T) {
	h, projectID := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	created, _, err := h.DB().CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "via uid",
		Author:    "tester",
	})
	require.NoError(t, err)

	resp, bs := getStatusBody(t, ts, "/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+created.UID)
	require.Equalf(t, 200, resp.StatusCode, "body: %s", string(bs))
	var body map[string]any
	require.NoError(t, json.Unmarshal(bs, &body))
	issue := body["issue"].(map[string]any)
	assert.Equal(t, created.UID, issue["uid"])
}

// TestGetIssue_ULIDInWrongProjectReturns404 pins the cross-project guard in
// resolveIssueRef: a ULID that belongs to project A must not be reachable
// via /api/v1/projects/{B}/issues/{ULID}. Without this guard a caller could
// probe other projects' issue existence via the wrong-project URL.
func TestGetIssue_ULIDInWrongProjectReturns404(t *testing.T) {
	h, projectA := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	ctx := context.Background()
	projectB, err := h.DB().CreateProject(ctx, "other-project")
	require.NoError(t, err)
	created, _, err := h.DB().CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projectA,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "lives in project A",
		Author:    "tester",
	})
	require.NoError(t, err)

	resp, bs := getStatusBody(t, ts, "/api/v1/projects/"+strconv.FormatInt(projectB.ID, 10)+"/issues/"+created.UID)
	assertAPIError(t, resp.StatusCode, bs, 404, "issue_not_found")
}

// TestGetIssue_QualifiedRefCrossProjectGuard pins the qualifier guard in
// resolveIssueRef: when the ref qualifies a project name, that name must
// match the URL's project_id's canonical name. Same-project qualifier
// resolves; different-project qualifier 404s — never silently substitutes
// a same-suffix issue in the URL project.
func TestGetIssue_QualifiedRefCrossProjectGuard(t *testing.T) {
	h, projectA := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	ctx := context.Background()
	pa, err := h.DB().ProjectByID(ctx, projectA)
	require.NoError(t, err)
	_, err = h.DB().CreateProject(ctx, "other-project")
	require.NoError(t, err)
	created, _, err := h.DB().CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projectA,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "lives in A",
		Author:    "tester",
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		name      string
		qualifier string
		wantOK    bool
	}{
		{"same project qualifier resolves", pa.Name, true},
		{"different project qualifier 404s", "other-project", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := "/api/v1/projects/" + strconv.FormatInt(projectA, 10) +
				"/issues/" + tc.qualifier + "%23" + created.ShortID
			resp, bs := getStatusBody(t, ts, url)
			if tc.wantOK {
				require.Equalf(t, 200, resp.StatusCode, "body: %s", string(bs))
			} else {
				assertAPIError(t, resp.StatusCode, bs, 404, "issue_not_found")
			}
		})
	}
}

// TestGetIssue_LegacyNumberReturns404 pins that legacy /issues/<int> requests
// (small integers below shortid.MinLength) 404. Spec §6: bare numeric refs
// like "12" no longer resolve after the cutover.
func TestGetIssue_LegacyNumberReturns404(t *testing.T) {
	h, projectID := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	_, _, err := h.DB().CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "no number lookup",
		Author:    "tester",
	})
	require.NoError(t, err)
	resp, _ := getStatusBody(t, ts, "/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/12")
	assert.Equal(t, 404, resp.StatusCode)
}

func TestIssues_CreateRoundtrip(t *testing.T) {
	h, projectID := bootstrapProject(t)
	resp, bs := postJSON(t, h.ts.(*httptest.Server), issuesURL(projectID),
		map[string]any{"actor": "agent-1", "title": "first", "body": "details"})
	require.Equal(t, 200, resp.StatusCode, string(bs))

	var body struct {
		Issue struct {
			ShortID string `json:"short_id"`
			Title   string `json:"title"`
			Status  string `json:"status"`
		} `json:"issue"`
		Event struct{ Type string } `json:"event"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.NotEmpty(t, body.Issue.ShortID, "short_id missing from create response")
	assert.Equal(t, "first", body.Issue.Title)
	assert.Equal(t, "open", body.Issue.Status)
	assert.Equal(t, "issue.created", body.Event.Type)
}

func TestIssues_UIDWireShapeAndLookup(t *testing.T) {
	h, projectID := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	resp, bs := postJSON(t, ts, issuesURL(projectID),
		map[string]any{"actor": "agent-1", "title": "uid issue"})
	require.Equal(t, 200, resp.StatusCode, string(bs))

	var created struct {
		Issue struct {
			UID        string `json:"uid"`
			ProjectUID string `json:"project_uid"`
			ShortID    string `json:"short_id"`
		} `json:"issue"`
		Event struct {
			ProjectUID string  `json:"project_uid"`
			IssueUID   *string `json:"issue_uid"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(bs, &created))
	assert.True(t, uid.Valid(created.Issue.UID))
	assert.True(t, uid.Valid(created.Issue.ProjectUID))
	assert.NotEmpty(t, created.Issue.ShortID)
	assert.Equal(t, created.Issue.ProjectUID, created.Event.ProjectUID)
	require.NotNil(t, created.Event.IssueUID)
	assert.Equal(t, created.Issue.UID, *created.Event.IssueUID)

	listBS := getBody(t, ts, issuesURL(projectID))
	assert.Contains(t, listBS, `"uid":"`+created.Issue.UID+`"`)
	assert.Contains(t, listBS, `"project_uid":"`+created.Issue.ProjectUID+`"`)

	byUIDResp, byUIDBS := getStatusBody(t, ts, "/api/v1/issues/"+created.Issue.UID)
	require.Equal(t, 200, byUIDResp.StatusCode, string(byUIDBS))
	assert.Contains(t, string(byUIDBS), `"short_id":"`+created.Issue.ShortID+`"`)
	assert.Contains(t, string(byUIDBS), `"uid":"`+created.Issue.UID+`"`)

	badResp, badBS := getStatusBody(t, ts, "/api/v1/issues/not-a-ulid")
	assert.Equal(t, 400, badResp.StatusCode, string(badBS))
	assert.Contains(t, string(badBS), `"code":"validation"`)
}

// TestCreateIssue_EventCarriesIssueShortID pins that the event returned in
// the create response carries issue_short_id, matching the wire shape that
// /events poll and SSE deliver. eventSelectByID must join issues to populate
// the short_id; without that join, mutation responses would silently omit
// the field while polled/streamed events have it.
func TestCreateIssue_EventCarriesIssueShortID(t *testing.T) {
	h, projectID := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	resp, bs := postJSON(t, ts, issuesURL(projectID),
		map[string]any{"actor": "agent-1", "title": "short id in event"})
	require.Equal(t, 200, resp.StatusCode, string(bs))

	var created struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
		Event struct {
			Type         string  `json:"type"`
			IssueShortID *string `json:"issue_short_id"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(bs, &created))
	require.NotEmpty(t, created.Issue.ShortID)
	assert.Equal(t, "issue.created", created.Event.Type)
	require.NotNil(t, created.Event.IssueShortID,
		"event in create response must carry issue_short_id (wire parity with poll/SSE)")
	assert.Equal(t, created.Issue.ShortID, *created.Event.IssueShortID)
}

func TestIssues_ListAndShow(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	for _, title := range []string{"a", "b"} {
		_, _ = postJSON(t, ts, issuesURL(pid),
			map[string]any{"actor": "x", "title": title})
	}

	listBS := getBody(t, ts, issuesURL(pid)+"?status=open")
	assert.Contains(t, listBS, `"title":"a"`)
	assert.Contains(t, listBS, `"title":"b"`)

	showBS := getBody(t, ts, issueURL(pid, 1, ""))
	assert.Contains(t, showBS, `"comments":`)
}

func TestIssues_ListMissingProjectIs404(t *testing.T) {
	h, _ := bootstrapProject(t)
	resp, bs := getStatusBody(t, h.ts.(*httptest.Server), "/api/v1/projects/9999/issues")
	assertAPIError(t, resp.StatusCode, bs, 404, "project_not_found")
}

func TestIssues_PatchEditTitleAndBody(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	_, _ = postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "x", "title": "old"})

	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""),
		map[string]any{"actor": "x", "title": "new"})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"title":"new"`)
}

// TestEditIssue_FieldOnlyResponseOmitsChanges pins the wire contract that
// `changes` is only present on responses to relationship-bearing PATCHes.
// A title-only edit must not serialize a `changes` key at all (older
// clients keyed off its presence to detect link mutations).
func TestEditIssue_FieldOnlyResponseOmitsChanges(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	_, _ = postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "x", "title": "old"})

	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""),
		map[string]any{"actor": "x", "title": "new"})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(bs, &raw))
	_, present := raw["changes"]
	assert.False(t, present, "field-only PATCH must not serialize a `changes` key, got: %s", string(bs))
}

// TestEditIssue_EmptyLinksDeltaResponseOmitsChanges pins the wire
// contract for the edge case where a client sends `links_delta: {}`
// (the field is present but no mutation requested). The response
// must NOT include `changes`, matching the field-only PATCH shape —
// the gate is "did the request actually ask for a link op", not
// "is the links_delta field non-nil". Otherwise older clients keying
// off the presence of `changes` would falsely classify these PATCHes
// as relationship mutations.
func TestEditIssue_EmptyLinksDeltaResponseOmitsChanges(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	_, _ = postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "x", "title": "old"})

	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor":       "x",
		"title":       "new",
		"links_delta": map[string]any{},
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(bs, &raw))
	_, present := raw["changes"]
	assert.False(t, present, "empty links_delta must not serialize a `changes` key, got: %s", string(bs))
}

func TestCreateIssue_BlankActorIs400(t *testing.T) {
	h, pid := bootstrapProject(t)
	resp, bs := postJSON(t, h.ts.(*httptest.Server), issuesURL(pid),
		map[string]any{"actor": "   ", "title": "x"})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

// TestEditIssue_LinksDelta_AddBlocks pins the smallest end-to-end behavior of
// the new PATCH-with-links shape: a single `add_blocks` entry in `links_delta`
// creates the corresponding link and reports it back in the response's
// `changes` block. This is the foundation the larger atomic PATCH builds on.
func TestEditIssue_LinksDelta_AddBlocks(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	// Create a second issue so we have a target to block.
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "blocked target"})
	require.Equalf(t, 200, resp.StatusCode, "create #2: %s", string(bs))
	var created struct {
		Issue struct {
			ID      int64  `json:"id"`
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(bs, &created))
	target := created.Issue.ShortID
	srcShortID := issueRefByID(t, h, src)

	resp, bs = patchJSON(t, ts, issueURL(pid, src, ""), map[string]any{
		"actor": "tester",
		"links_delta": map[string]any{
			"add_blocks": []string{target},
		},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
		Changes struct {
			BlocksAdded []linkPeerTest `json:"blocks_added"`
		} `json:"changes"`
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	assert.Equal(t, srcShortID, out.Issue.ShortID)
	assert.True(t, out.Changed, "changed flag should be true")
	require.Len(t, out.Changes.BlocksAdded, 1)
	assert.Equal(t, target, out.Changes.BlocksAdded[0].ShortID)

	// Verify the link persisted: GET issue and inspect its links list.
	getResp, err := http.Get(ts.URL + issueURL(pid, src, "")) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	require.Equal(t, 200, getResp.StatusCode)
	var show struct {
		Links []struct {
			Type string       `json:"type"`
			From linkPeerTest `json:"from"`
			To   linkPeerTest `json:"to"`
		} `json:"links"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&show))
	require.Len(t, show.Links, 1)
	assert.Equal(t, "blocks", show.Links[0].Type)
	assert.Equal(t, srcShortID, show.Links[0].From.ShortID)
	assert.Equal(t, target, show.Links[0].To.ShortID)
}

// TestEditIssue_LinksDelta_AddBlockedBy verifies the inverse-direction add:
// `add_blocked_by: [N]` on URL issue X stores a `blocks` link from N to X
// (i.e. N blocks X). The Changes block reports it under blocked_by_added.
func TestEditIssue_LinksDelta_AddBlockedBy(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "blocker"})
	require.Equal(t, 200, resp.StatusCode)
	var created struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(bs, &created))
	blocker := created.Issue.ShortID
	srcShortID := issueRefByID(t, h, src)

	resp, bs = patchJSON(t, ts, issueURL(pid, src, ""), map[string]any{
		"actor": "tester",
		"links_delta": map[string]any{
			"add_blocked_by": []string{blocker},
		},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changes struct {
			BlockedByAdded []linkPeerTest `json:"blocked_by_added"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.Len(t, out.Changes.BlockedByAdded, 1)
	assert.Equal(t, blocker, out.Changes.BlockedByAdded[0].ShortID)

	// Persistence: GET URL issue, expect a blocks link FROM blocker TO src.
	getResp, err := http.Get(ts.URL + issueURL(pid, src, "")) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	var show struct {
		Links []struct {
			Type string       `json:"type"`
			From linkPeerTest `json:"from"`
			To   linkPeerTest `json:"to"`
		} `json:"links"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&show))
	require.Len(t, show.Links, 1)
	assert.Equal(t, "blocks", show.Links[0].Type)
	assert.Equal(t, blocker, show.Links[0].From.ShortID)
	assert.Equal(t, srcShortID, show.Links[0].To.ShortID)
}

// TestEditIssue_LinksDelta_AddRelated covers the symmetric link type. The
// related link is canonical-ordered server-side; the Changes block reports
// the target as the agent passed it (issue ref from the operating issue's
// POV), regardless of canonical storage order.
func TestEditIssue_LinksDelta_AddRelated(t *testing.T) {
	_, ts, pid, src := bootstrapProjectWithIssue(t)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "other"})
	require.Equal(t, 200, resp.StatusCode)
	var created struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(bs, &created))
	other := created.Issue.ShortID

	resp, bs = patchJSON(t, ts, issueURL(pid, src, ""), map[string]any{
		"actor": "tester",
		"links_delta": map[string]any{
			"add_related": []string{other},
		},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changes struct {
			RelatedAdded []linkPeerTest `json:"related_added"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.Len(t, out.Changes.RelatedAdded, 1)
	assert.Equal(t, other, out.Changes.RelatedAdded[0].ShortID)
}

// TestEditIssue_LinksDelta_SetParent sets the parent slot. With no existing
// parent, set_parent inserts a parent link. Changes reports parent_set.
func TestEditIssue_LinksDelta_SetParent(t *testing.T) {
	_, ts, pid, src := bootstrapProjectWithIssue(t)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "parent"})
	require.Equal(t, 200, resp.StatusCode)
	var created struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(bs, &created))
	parentRef := created.Issue.ShortID

	resp, bs = patchJSON(t, ts, issueURL(pid, src, ""), map[string]any{
		"actor": "tester",
		"links_delta": map[string]any{
			"set_parent": parentRef,
		},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changes struct {
			ParentSet *linkPeerTest `json:"parent_set"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.NotNil(t, out.Changes.ParentSet)
	assert.Equal(t, parentRef, out.Changes.ParentSet.ShortID)
}

// TestEditIssue_LinksDelta_RemoveBlocksAfterPeerSoftDeleted pins that
// soft-deleting one end of a link does NOT prevent the other end from
// removing the link via `kata edit --remove-blocks N`. The link row is
// real and the user can still ask to clean it up; the peer's open/
// closed/deleted state is irrelevant for addressing the link.
func TestEditIssue_LinksDelta_RemoveBlocksAfterPeerSoftDeleted(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "peer"})
	require.Equal(t, 200, resp.StatusCode)
	peer := issueShortIDFromCreate(t, bs)
	srcShortID := issueRefByID(t, h, src)
	project, err := h.DB().ProjectByID(context.Background(), pid)
	require.NoError(t, err)

	// src blocks peer.
	resp, _ = patchJSON(t, ts, issueURL(pid, src, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"add_blocks": []string{peer}},
	})
	require.Equal(t, 200, resp.StatusCode)

	// Soft-delete peer.
	delResp := postWithHeader(t, ts, issueURLRef(pid, peer, "actions/delete"),
		map[string]string{"X-Kata-Confirm": "DELETE " + project.Name + "#" + peer},
		map[string]any{"actor": "tester"})
	require.Equalf(t, 200, delResp.status, "delete: %s", string(delResp.body))

	// Now remove the link from src's side. Without soft-delete-tolerance,
	// the per-target lookup returned ErrNotFound and the remove silently
	// no-op'd, leaving the link orphaned.
	rmResp, rmBs := patchJSON(t, ts, issueURLRef(pid, srcShortID, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"remove_blocks": []string{peer}},
	})
	require.Equalf(t, 200, rmResp.StatusCode, "remove: %s", string(rmBs))
	var out struct {
		Changes struct {
			BlocksRemoved []linkPeerTest `json:"blocks_removed"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(rmBs, &out))
	require.Len(t, out.Changes.BlocksRemoved, 1,
		"the link to a soft-deleted peer must still be removable")
	assert.Equal(t, peer, out.Changes.BlocksRemoved[0].ShortID)
}

// TestShowIssue_LinkPeerSoftDeletedReturns200 pins that GET on an
// issue whose linked peer is soft-deleted still returns the surviving
// issue successfully. Iteration-22 surfaced that the show path was
// thought to be using a deleted-tolerant lookup; this test makes the
// promise explicit so a future regression can't silently reintroduce
// a 500.
func TestShowIssue_LinkPeerSoftDeletedReturns200(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "peer"})
	require.Equal(t, 200, resp.StatusCode)
	peer := issueShortIDFromCreate(t, bs)
	srcShortID := issueRefByID(t, h, src)
	project, err := h.DB().ProjectByID(context.Background(), pid)
	require.NoError(t, err)

	// src blocks peer.
	resp, _ = patchJSON(t, ts, issueURLRef(pid, srcShortID, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"add_blocks": []string{peer}},
	})
	require.Equal(t, 200, resp.StatusCode)

	// Soft-delete peer.
	delResp := postWithHeader(t, ts, issueURLRef(pid, peer, "actions/delete"),
		map[string]string{"X-Kata-Confirm": "DELETE " + project.Name + "#" + peer},
		map[string]any{"actor": "tester"})
	require.Equalf(t, 200, delResp.status, "delete: %s", string(delResp.body))

	// Show src — the surviving end. Must succeed (200) and surface the
	// link to the soft-deleted peer; the peer's short_id rides on the
	// link row even though its issue row is hidden.
	getResp, err := http.Get(ts.URL + issueURLRef(pid, srcShortID, "")) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	showBody, err := io.ReadAll(getResp.Body)
	require.NoError(t, err)
	require.Equalf(t, 200, getResp.StatusCode, "show survivor must not 500: %s", string(showBody))
	var out struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
		Links []struct {
			Type string       `json:"type"`
			To   linkPeerTest `json:"to"`
		} `json:"links"`
	}
	require.NoError(t, json.Unmarshal(showBody, &out))
	assert.Equal(t, srcShortID, out.Issue.ShortID)
	require.Len(t, out.Links, 1, "link to soft-deleted peer must still appear")
	assert.Equal(t, "blocks", out.Links[0].Type)
	assert.Equal(t, peer, out.Links[0].To.ShortID)
}

// TestShowIssue_ParentPeerSoftDeletedReturns200 pins the same property
// for the parent slot: a child whose parent is soft-deleted must still
// render through GET. Companion of the link-peer test above.
func TestShowIssue_ParentPeerSoftDeletedReturns200(t *testing.T) {
	h, ts, pid, par := bootstrapProjectWithIssue(t)
	parRef := issueRefByID(t, h, par)
	// Seeded issue acts as the future parent. Make a child issue, set its
	// parent to the seeded one, then soft-delete the seeded one.
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "child"})
	require.Equal(t, 200, resp.StatusCode)
	childRef := issueShortIDFromCreate(t, bs)
	project, err := h.DB().ProjectByID(context.Background(), pid)
	require.NoError(t, err)

	resp, _ = patchJSON(t, ts, issueURLRef(pid, childRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"set_parent": parRef},
	})
	require.Equal(t, 200, resp.StatusCode)
	delResp := postWithHeader(t, ts, issueURLRef(pid, parRef, "actions/delete"),
		map[string]string{"X-Kata-Confirm": "DELETE " + project.Name + "#" + parRef},
		map[string]any{"actor": "tester"})
	require.Equalf(t, 200, delResp.status, "delete: %s", string(delResp.body))

	getResp, err := http.Get(ts.URL + issueURLRef(pid, childRef, "")) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	showBody, err := io.ReadAll(getResp.Body)
	require.NoError(t, err)
	require.Equalf(t, 200, getResp.StatusCode, "child show must not 500 with soft-deleted parent: %s", string(showBody))
	var out struct {
		Parent *struct {
			ShortID string `json:"short_id"`
		} `json:"parent"`
	}
	require.NoError(t, json.Unmarshal(showBody, &out))
	require.NotNil(t, out.Parent, "soft-deleted parent must still surface in show")
	assert.Equal(t, parRef, out.Parent.ShortID)
}

// TestEditIssue_LinksDelta_SetParent_RejectsCycle pins that set_parent
// rejects an edit that would create a parent cycle. Builds a graph where
// child's parent = src (so src is an ancestor of child) and asks src to
// set its parent to child; the edit must fail with 400 validation, leaving
// the existing parent state untouched.
func TestEditIssue_LinksDelta_SetParent_RejectsCycle(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "child"})
	require.Equal(t, 200, resp.StatusCode)
	childRef := issueShortIDFromCreate(t, bs)
	// child's parent = src.
	resp, _ = patchJSON(t, ts, issueURLRef(pid, childRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"set_parent": srcRef},
	})
	require.Equal(t, 200, resp.StatusCode)

	// Now ask src to set its parent to child — that would create the cycle.
	resp, bs = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"set_parent": childRef},
	})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")

	// src must still have no parent (rollback semantics).
	getResp, err := http.Get(ts.URL + issueURLRef(pid, srcRef, "")) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	var show struct {
		Parent any `json:"parent,omitempty"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&show))
	assert.Nil(t, show.Parent, "src's parent must remain unset after the cycle rejection")
}

// TestEditIssue_LinksDelta_SetParent_ReplaceRecordsBoth pins that
// replacing an existing parent surfaces BOTH parent_set (new) and
// parent_removed (old) in the changes block. Without this, consumers
// of issue.links_changed and the digest accounting see a parent
// replace as a pure add and lose track of the prior parent.
func TestEditIssue_LinksDelta_SetParent_ReplaceRecordsBoth(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "old-parent"})
	require.Equal(t, 200, resp.StatusCode)
	oldParent := issueShortIDFromCreate(t, bs)
	resp, bs = postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "new-parent"})
	require.Equal(t, 200, resp.StatusCode)
	newParent := issueShortIDFromCreate(t, bs)

	// Set src's parent to oldParent.
	resp, _ = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"set_parent": oldParent},
	})
	require.Equal(t, 200, resp.StatusCode)

	// Replace: set parent to newParent. The change payload must list both
	// newParent (set) and oldParent (removed).
	resp, bs = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"set_parent": newParent},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changes struct {
			ParentSet     *linkPeerTest `json:"parent_set"`
			ParentRemoved *linkPeerTest `json:"parent_removed"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.NotNil(t, out.Changes.ParentSet, "parent_set must be present on replace")
	assert.Equal(t, newParent, out.Changes.ParentSet.ShortID)
	require.NotNil(t, out.Changes.ParentRemoved, "parent_removed must be present on replace")
	assert.Equal(t, oldParent, out.Changes.ParentRemoved.ShortID)
}

// TestEditIssue_LinksDelta_RemoveParent_StrictSuccess removes the parent
// link when the asserted current parent matches reality.
func TestEditIssue_LinksDelta_RemoveParent_StrictSuccess(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "parent"})
	require.Equal(t, 200, resp.StatusCode)
	parentRef := issueShortIDFromCreate(t, bs)
	// Set src's parent to parentRef first.
	resp, _ = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"set_parent": parentRef},
	})
	require.Equal(t, 200, resp.StatusCode)

	// Remove asserting parentRef. Must succeed.
	resp, bs = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"remove_parent": parentRef},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changes struct {
			ParentRemoved *linkPeerTest `json:"parent_removed"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.NotNil(t, out.Changes.ParentRemoved)
	assert.Equal(t, parentRef, out.Changes.ParentRemoved.ShortID)
}

// TestEditIssue_LinksDelta_RemoveParent_MismatchIs409 fails loudly when the
// asserted parent does not match the current parent. This is the optimistic-
// concurrency safety check that protects agents acting on stale state.
func TestEditIssue_LinksDelta_RemoveParent_MismatchIs409(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "parent"})
	require.Equal(t, 200, resp.StatusCode)
	parentRef := issueShortIDFromCreate(t, bs)
	resp, bs = postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "wrong-parent"})
	require.Equal(t, 200, resp.StatusCode)
	wrongRef := issueShortIDFromCreate(t, bs)
	resp, _ = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"set_parent": parentRef},
	})
	require.Equal(t, 200, resp.StatusCode)

	// Assert wrongRef even though current parent is parentRef → 409.
	resp, bs = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"remove_parent": wrongRef},
	})
	assertAPIError(t, resp.StatusCode, bs, 409, "parent_mismatch")
}

// TestEditIssue_LinksDelta_RemoveBlocks removes a `blocks` link from the
// URL issue to the target. Idempotent: removing a missing link is a no-op
// (handled by a separate test).
func TestEditIssue_LinksDelta_RemoveBlocks(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "blocked"})
	require.Equal(t, 200, resp.StatusCode)
	target := issueShortIDFromCreate(t, bs)
	resp, _ = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"add_blocks": []string{target}},
	})
	require.Equal(t, 200, resp.StatusCode)

	resp, bs = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"remove_blocks": []string{target}},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changes struct {
			BlocksRemoved []linkPeerTest `json:"blocks_removed"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.Len(t, out.Changes.BlocksRemoved, 1)
	assert.Equal(t, target, out.Changes.BlocksRemoved[0].ShortID)
}

// TestEditIssue_LinksDelta_RemoveBlocks_NonexistentTargetIsNoop pins
// the contract that the idempotent --remove-blocks /-blocked-by /
// -related path treats "target issue doesn't exist" the same as
// "edge doesn't exist": both succeed as no-ops. The desired end state
// — "no link from this issue to N" — already holds when there is no
// N at all. This matches the symmetry the CLI documents under
// "(idempotent)". Strict --remove-parent retains its loud-failure
// semantics; that path asserts a fact about the current parent and
// is covered by the parent-mismatch test elsewhere.
func TestEditIssue_LinksDelta_RemoveBlocks_NonexistentTargetIsNoop(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)

	resp, bs := patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"remove_blocks": []string{"zzzz"}},
	})
	require.Equalf(t, 200, resp.StatusCode, "remove against missing target must succeed: %s", string(bs))

	var out struct {
		Changes struct {
			BlocksRemoved []linkPeerTest `json:"blocks_removed"`
		} `json:"changes"`
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	assert.Empty(t, out.Changes.BlocksRemoved, "no edge → no change reported")
	assert.False(t, out.Changed, "missing-target idempotent remove must not flip changed")
}

// TestEditIssue_LinksDelta_RemoveBlocks_IdempotentNoop succeeds and reports
// no changes when the link doesn't exist.
func TestEditIssue_LinksDelta_RemoveBlocks_IdempotentNoop(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "other"})
	require.Equal(t, 200, resp.StatusCode)
	other := issueShortIDFromCreate(t, bs)

	resp, bs = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"remove_blocks": []string{other}},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changes struct {
			BlocksRemoved []linkPeerTest `json:"blocks_removed"`
		} `json:"changes"`
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	assert.Empty(t, out.Changes.BlocksRemoved)
	assert.False(t, out.Changed, "no-op remove should not flip changed")
}

// TestEditIssue_LinksDelta_RemoveBlockedBy removes the inverse-direction
// link: a `blocks` link FROM the target TO the URL issue.
func TestEditIssue_LinksDelta_RemoveBlockedBy(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "blocker"})
	require.Equal(t, 200, resp.StatusCode)
	blocker := issueShortIDFromCreate(t, bs)
	resp, _ = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"add_blocked_by": []string{blocker}},
	})
	require.Equal(t, 200, resp.StatusCode)

	resp, bs = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"remove_blocked_by": []string{blocker}},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changes struct {
			BlockedByRemoved []linkPeerTest `json:"blocked_by_removed"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.Len(t, out.Changes.BlockedByRemoved, 1)
	assert.Equal(t, blocker, out.Changes.BlockedByRemoved[0].ShortID)
}

// TestEditIssue_LinksDelta_RemoveRelated removes a related link.
// Storage canonicalization is invisible to the caller.
func TestEditIssue_LinksDelta_RemoveRelated(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "other"})
	require.Equal(t, 200, resp.StatusCode)
	other := issueShortIDFromCreate(t, bs)
	resp, _ = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"add_related": []string{other}},
	})
	require.Equal(t, 200, resp.StatusCode)

	resp, bs = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"remove_related": []string{other}},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changes struct {
			RelatedRemoved []linkPeerTest `json:"related_removed"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.Len(t, out.Changes.RelatedRemoved, 1)
	assert.Equal(t, other, out.Changes.RelatedRemoved[0].ShortID)
}

// TestEditIssue_LinksDelta_ConflictAddRemoveSameTarget rejects a delta that
// asks to both add and remove the same (type, target) pair in one call.
// Caught client-side-of-DB before any link mutation runs.
func TestEditIssue_LinksDelta_ConflictAddRemoveSameTarget(t *testing.T) {
	_, ts, pid, _ := bootstrapProjectWithIssue(t)
	resp, _ := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "target"})
	require.Equal(t, 200, resp.StatusCode)

	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor": "tester",
		"links_delta": map[string]any{
			"add_blocks":    []int64{2},
			"remove_blocks": []int64{2},
		},
	})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

// TestEditIssue_LinksDelta_ConflictParentBoth rejects set_parent + remove_parent
// in the same call.
func TestEditIssue_LinksDelta_ConflictParentBoth(t *testing.T) {
	_, ts, pid, _ := bootstrapProjectWithIssue(t)
	resp, _ := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "p"})
	require.Equal(t, 200, resp.StatusCode)

	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor": "tester",
		"links_delta": map[string]any{
			"set_parent":    int64(2),
			"remove_parent": int64(2),
		},
	})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

// TestEditIssue_LinksDelta_SelfLinkRejected rejects an add that targets the
// URL issue itself.
func TestEditIssue_LinksDelta_SelfLinkRejected(t *testing.T) {
	_, ts, pid, _ := bootstrapProjectWithIssue(t)
	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor":       "tester",
		"links_delta": map[string]any{"add_blocks": []int64{1}},
	})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

// TestEditIssue_Priority_SetViaPATCH sets priority via PATCH with set_priority
// instead of the legacy priority action endpoint.
func TestEditIssue_Priority_SetViaPATCH(t *testing.T) {
	_, ts, pid, _ := bootstrapProjectWithIssue(t)
	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor":        "tester",
		"set_priority": int64(1),
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	getResp, err := http.Get(ts.URL + issueURL(pid, 1, "")) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	var show struct {
		Issue struct {
			Priority *int64 `json:"priority"`
		} `json:"issue"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&show))
	require.NotNil(t, show.Issue.Priority)
	assert.Equal(t, int64(1), *show.Issue.Priority)
}

// TestEditIssue_Priority_ClearViaPATCH clears priority via clear_priority=true.
func TestEditIssue_Priority_ClearViaPATCH(t *testing.T) {
	_, ts, pid, _ := bootstrapProjectWithIssue(t)
	resp, _ := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor":        "tester",
		"set_priority": int64(2),
	})
	require.Equal(t, 200, resp.StatusCode)

	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor":          "tester",
		"clear_priority": true,
	})
	require.Equalf(t, 200, resp.StatusCode, "clear: %s", string(bs))

	getResp, err := http.Get(ts.URL + issueURL(pid, 1, "")) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	var show struct {
		Issue struct {
			Priority *int64 `json:"priority"`
		} `json:"issue"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&show))
	assert.Nil(t, show.Issue.Priority)
}

// TestEditIssue_Priority_BothFlagsRejected rejects passing both set_priority
// and clear_priority in one call.
func TestEditIssue_Priority_BothFlagsRejected(t *testing.T) {
	_, ts, pid, _ := bootstrapProjectWithIssue(t)
	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor":          "tester",
		"set_priority":   int64(2),
		"clear_priority": true,
	})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

// TestEditIssue_FieldsUnchanged_NoOp covers an iteration-11 finding:
// a PATCH that includes a field flag whose value already matches the
// row must NOT emit issue.updated, must NOT bump updated_at, and must
// NOT report changed=true. Without this, a request like
// `kata edit X --title "(current title)" --remove-blocks Y` fires
// hooks/digest activity for a non-mutation, breaking the
// idempotency contract the patch establishes.
func TestEditIssue_FieldsUnchanged_NoOp(t *testing.T) {
	_, ts, pid, _ := bootstrapProjectWithIssue(t)

	cursor := highestEventID(t, ts, pid)
	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor": "tester",
		"title": "x", // matches the seed title from bootstrapProjectWithIssue
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	assert.False(t, out.Changed,
		"a no-op title (already current) must not flip changed=true")

	events := eventsAfter(t, ts, pid, cursor)
	for _, e := range events {
		assert.NotEqualf(t, "issue.updated", e.Type,
			"a no-op title must not emit issue.updated")
	}
}

// TestEditIssue_AggregatedEvent_PayloadCarriesUIDs pins that the
// aggregated payload includes stable UIDs for every referenced peer in
// addition to the user-friendly short_ids. UIDs are required so events
// remain meaningful after a federation merge re-extends a peer's
// short_id; UIDs cannot collide.
func TestEditIssue_AggregatedEvent_PayloadCarriesUIDs(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "parent"})
	require.Equal(t, 200, resp.StatusCode)
	parentRef := issueShortIDFromCreate(t, bs)
	resp, bs = postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "block-target"})
	require.Equal(t, 200, resp.StatusCode)
	blockTarget := issueShortIDFromCreate(t, bs)

	cursor := highestEventID(t, ts, pid)
	resp, _ = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor": "tester",
		"links_delta": map[string]any{
			"set_parent": parentRef,
			"add_blocks": []string{blockTarget},
		},
	})
	require.Equal(t, 200, resp.StatusCode)

	events := eventsAfter(t, ts, pid, cursor)
	var sawUIDs bool
	for _, e := range events {
		if e.Type != "issue.links_changed" {
			continue
		}
		payload := e.PayloadString()
		// Both short_id and UID fields must be present.
		assert.Contains(t, payload, `"parent_set":"`+parentRef+`"`)
		assert.Contains(t, payload, `"parent_set_uid":`)
		assert.Contains(t, payload, `"blocks_added":["`+blockTarget+`"]`)
		assert.Contains(t, payload, `"blocks_added_uids":[`)
		sawUIDs = true
	}
	require.True(t, sawUIDs, "issue.links_changed event must include UID-keyed fields")
}

// TestEditIssue_Response_ExposesAllEvents covers an iteration-13 roborev
// finding: a PATCH that touches multiple event classes (e.g. priority +
// links) used to expose only the LAST emitted event in the response,
// silently hiding the priority transition from any client that read
// `event` rather than the events stream. The response now carries
// `events: []` with every emitted event in order; `event` is retained
// pointing at the final entry as a back-compat alias.
func TestEditIssue_Response_ExposesAllEvents(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "target"})
	require.Equal(t, 200, resp.StatusCode)
	target := issueShortIDFromCreate(t, bs)

	resp, bs = patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor":        "tester",
		"set_priority": int64(1),
		"links_delta":  map[string]any{"add_blocks": []string{target}},
	})
	require.Equalf(t, 200, resp.StatusCode, "patch: %s", string(bs))

	var out struct {
		Event *struct {
			Type string `json:"type"`
		} `json:"event"`
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.GreaterOrEqualf(t, len(out.Events), 2, "events must include both transitions: %s", string(bs))

	types := make([]string, 0, len(out.Events))
	for _, e := range out.Events {
		types = append(types, e.Type)
	}
	assert.Contains(t, types, "issue.priority_set")
	assert.Contains(t, types, "issue.links_changed")
	require.NotNil(t, out.Event)
	assert.Equal(t, types[len(types)-1], out.Event.Type,
		"event compatibility alias must point at the last emitted event")
}

// TestEditIssue_AggregatedEvent_OnePerEdit verifies that one PATCH with
// multiple link mutations produces exactly one issue.links_changed event
// (not one event per link). The payload lists every applied add and remove.
func TestEditIssue_AggregatedEvent_OnePerEdit(t *testing.T) {
	h, ts, pid, src := bootstrapProjectWithIssue(t)
	srcRef := issueRefByID(t, h, src)
	refs := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		resp, bs := postJSON(t, ts, issuesURL(pid),
			map[string]any{"actor": "tester", "title": fmt.Sprintf("t%d", i)})
		require.Equal(t, 200, resp.StatusCode)
		refs = append(refs, issueShortIDFromCreate(t, bs))
	}

	// Snapshot the highest-known event ID before the PATCH so we can read
	// only the events emitted by the edit under test.
	cursorBefore := highestEventID(t, ts, pid)

	resp, _ := patchJSON(t, ts, issueURLRef(pid, srcRef, ""), map[string]any{
		"actor": "tester",
		"links_delta": map[string]any{
			"add_blocks":     []string{refs[0], refs[1]},
			"add_blocked_by": []string{refs[2]},
			"add_related":    []string{refs[3]},
		},
	})
	require.Equal(t, 200, resp.StatusCode)

	events := eventsAfter(t, ts, pid, cursorBefore)
	var linkEventCount int
	var sawAggregated bool
	for _, e := range events {
		switch e.Type {
		case "issue.links_changed":
			sawAggregated = true
			payload := e.PayloadString()
			assert.Contains(t, payload, `"blocks_added":["`+refs[0]+`","`+refs[1]+`"]`)
			assert.Contains(t, payload, `"blocked_by_added":["`+refs[2]+`"]`)
			assert.Contains(t, payload, `"related_added":["`+refs[3]+`"]`)
		case "issue.linked", "issue.unlinked":
			linkEventCount++
		}
	}
	assert.True(t, sawAggregated, "one issue.links_changed event must be emitted")
	assert.Zero(t, linkEventCount, "no per-link issue.linked/issue.unlinked events on PATCH path")
}

// eventTransport is the minimal projection we read from the events list
// endpoint for assertion purposes. Payload arrives as a JSON object on
// the wire (see api.EventOut), so we hold it as RawMessage and convert
// to string only for substring assertions.
type eventTransport struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func (e eventTransport) PayloadString() string { return string(e.Payload) }

// highestEventID returns the largest event ID currently visible for the
// project; subsequent eventsAfter calls use this as the cursor.
func highestEventID(t *testing.T, ts *httptest.Server, pid int64) int64 {
	t.Helper()
	resp, err := http.Get(ts.URL + fmt.Sprintf("/api/v1/projects/%d/events?after_id=0&limit=1000", pid)) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var b struct {
		Events []eventTransport `json:"events"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&b))
	if len(b.Events) == 0 {
		return 0
	}
	return b.Events[len(b.Events)-1].ID
}

// eventsAfter returns every event for the project with ID > after.
func eventsAfter(t *testing.T, ts *httptest.Server, pid int64, after int64) []eventTransport {
	t.Helper()
	resp, err := http.Get(ts.URL + fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d&limit=1000", pid, after)) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var b struct {
		Events []eventTransport `json:"events"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&b))
	return b.Events
}

// TestEditIssue_Atomic_RollsBackOnLinkFailure verifies that a delta
// containing both a valid field change and a link op that fails (here:
// add_blocks targeting a missing issue → 404) leaves the issue completely
// unchanged. The field change must NOT land.
func TestEditIssue_Atomic_RollsBackOnLinkFailure(t *testing.T) {
	_, ts, pid, _ := bootstrapProjectWithIssue(t)
	resp, _ := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "tester", "title": "neighbor"})
	require.Equal(t, 200, resp.StatusCode)

	// Edit #1 with both a title change AND a link to a nonexistent #99.
	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""), map[string]any{
		"actor": "tester",
		"title": "must-not-stick",
		"links_delta": map[string]any{
			"add_blocks": []int64{2, 99}, // 99 is missing → must fail
		},
	})
	require.GreaterOrEqualf(t, resp.StatusCode, 400, "missing target must fail: %s", string(bs))

	// Title must NOT have changed; no link to #2 either. The whole edit rolls back.
	getResp, err := http.Get(ts.URL + issueURL(pid, 1, "")) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = getResp.Body.Close() }()
	var show struct {
		Issue struct {
			Title string `json:"title"`
		} `json:"issue"`
		Links []struct {
			Type string `json:"type"`
		} `json:"links"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&show))
	assert.NotEqual(t, "must-not-stick", show.Issue.Title,
		"title change must roll back when a same-call link op fails")
	assert.Empty(t, show.Links, "no link should have been committed")
}

func TestEditIssue_BlankActorIs400(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	_, _ = postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "x", "title": "old"})

	resp, bs := patchJSON(t, ts, issueURL(pid, 1, ""),
		map[string]any{"actor": "   ", "title": "new"})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

func TestCreateIssue_WithInitialState(t *testing.T) {
	env := testenv.New(t)
	pid, parent, _ := setupTwoIssues(t, env)
	parentRef := refForIssue(t, env, parent)

	var out struct {
		Issue struct {
			ShortID string  `json:"short_id"`
			Owner   *string `json:"owner"`
		} `json:"issue"`
		Event struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		} `json:"event"`
	}
	envPostJSON(t, env, projectPath(pid)+"/issues", map[string]any{
		"actor":  "tester",
		"title":  "child",
		"owner":  "alice",
		"labels": []string{"bug", "needs-review"},
		"links":  []map[string]any{{"type": "parent", "to_ref": parentRef}},
	}, &out)
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "alice", *out.Issue.Owner)
	assert.Equal(t, "issue.created", out.Event.Type)
	assert.Contains(t, out.Event.Payload, `"labels":["bug","needs-review"]`)
	assert.Contains(t, out.Event.Payload, `"owner":"alice"`)
	assert.Contains(t, out.Event.Payload, `"type":"parent"`)
}

func TestCreateIssue_InitialLinkToMissingTargetIs404(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	resp, _ := envDoRaw(t, env, http.MethodPost, projectPath(pid)+"/issues", map[string]any{
		"actor": "tester", "title": "child",
		"links": []map[string]any{{"type": "parent", "to_ref": "zzzz"}},
	}, nil)
	assert.Equal(t, 404, resp.StatusCode)
}

// TestCreateIssue_RejectsArchivedProject pins that creating an issue
// against an archived project returns 404 (matching the "archived
// projects are gone" semantic) rather than a 500. The DB-layer
// CreateIssue gates writes with deleted_at IS NULL and returns
// ErrNotFound; the handler must surface that as project_not_found
// after also rejecting in the preflight ProjectByID check.
func TestCreateIssue_RejectsArchivedProject(t *testing.T) {
	h, projectID := bootstrapProject(t)
	_, _, err := h.DB().RemoveProject(t.Context(), db.RemoveProjectParams{
		ProjectID: projectID, Actor: "tester",
	})
	require.NoError(t, err)

	resp, bs := postJSON(t, h.ts.(*httptest.Server), issuesURL(projectID),
		map[string]any{"actor": "agent-1", "title": "should fail", "body": "details"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusNotFound, "project_not_found")
}

func TestCreateIssue_InvalidLabelIs400(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	resp, _ := envDoRaw(t, env, http.MethodPost, projectPath(pid)+"/issues", map[string]any{
		"actor": "tester", "title": "x",
		"labels": []string{"BadCase"},
	}, nil)
	assert.Equal(t, 400, resp.StatusCode)
}

// TestCreateIssue_InitialSelfLinkIs400 verifies that an initial link whose
// to_number equals the issue being created (which lands as #1 in a fresh
// project) surfaces as a 400 validation error, not a 500. The DB layer
// catches this via ErrSelfLink and the handler must map it.
func TestCreateIssue_InitialSelfLinkIs400(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	resp, _ := envDoRaw(t, env, http.MethodPost, projectPath(pid)+"/issues", map[string]any{
		"actor": "tester", "title": "self",
		"links": []map[string]any{{"type": "parent", "to_number": 1}},
	}, nil)
	assert.Equal(t, 400, resp.StatusCode)
}

// TestCreate_IdempotencyReuse_SameFingerprint verifies that a second create
// with the same Idempotency-Key + same body returns the reuse envelope: no
// fresh event, the original_event populated, changed=false, reused=true.
func TestCreate_IdempotencyReuse_SameFingerprint(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	path := issuesURL(pid)
	body := map[string]any{"actor": "agent-1", "title": "fix login crash", "body": "stack trace here"}

	first := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "K1"}, body)
	requireOK(t, first)
	var firstOut struct {
		Issue struct {
			Number     int64  `json:"number"`
			UID        string `json:"uid"`
			ProjectUID string `json:"project_uid"`
		} `json:"issue"`
		Event struct {
			ID         int64   `json:"id"`
			ProjectUID string  `json:"project_uid"`
			IssueUID   *string `json:"issue_uid"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(first.body, &firstOut))
	require.NotZero(t, firstOut.Event.ID)

	second := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "K1"}, body)
	requireOK(t, second)
	var secondOut struct {
		Issue struct {
			Number int64 `json:"number"`
		} `json:"issue"`
		Event         *struct{ ID int64 } `json:"event"`
		OriginalEvent *struct {
			ID         int64   `json:"id"`
			ProjectUID string  `json:"project_uid"`
			IssueUID   *string `json:"issue_uid"`
		} `json:"original_event"`
		Changed bool `json:"changed"`
		Reused  bool `json:"reused"`
	}
	require.NoError(t, json.Unmarshal(second.body, &secondOut))
	assert.Equal(t, firstOut.Issue.Number, secondOut.Issue.Number)
	assert.Nil(t, secondOut.Event, "reuse must omit fresh event")
	require.NotNil(t, secondOut.OriginalEvent, "reuse must populate original_event")
	assert.Equal(t, firstOut.Event.ID, secondOut.OriginalEvent.ID)
	assert.Equal(t, firstOut.Issue.ProjectUID, secondOut.OriginalEvent.ProjectUID)
	require.NotNil(t, secondOut.OriginalEvent.IssueUID)
	assert.Equal(t, firstOut.Issue.UID, *secondOut.OriginalEvent.IssueUID)
	assert.False(t, secondOut.Changed)
	assert.True(t, secondOut.Reused)
}

// TestCreate_IdempotencyMismatch verifies that reusing the same Idempotency-Key
// with a different fingerprint (different title) is a 409 idempotency_mismatch.
func TestCreate_IdempotencyMismatch(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	path := issuesURL(pid)

	first := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "K1"},
		map[string]any{"actor": "agent-1", "title": "first title", "body": "first body"})
	requireOK(t, first)

	second := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "K1"},
		map[string]any{"actor": "agent-1", "title": "different title", "body": "different body"})
	require.Equal(t, 409, second.status, string(second.body))
	assert.Contains(t, string(second.body), `"code":"idempotency_mismatch"`)
	// Decode the response and pin the short_id echo so a future regression
	// that drops the data field surfaces immediately. The wire envelope is
	// {status, error: {code, message, data: {...}}}.
	var errBody struct {
		Error struct {
			Data struct {
				ShortID     string `json:"short_id"`
				UID         string `json:"uid"`
				QualifiedID string `json:"qualified_id"`
			} `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(second.body, &errBody), string(second.body))
	assert.NotEmpty(t, errBody.Error.Data.ShortID,
		"mismatch payload must echo the original issue's short_id")
	assert.NotEmpty(t, errBody.Error.Data.UID, "mismatch payload must echo UID")
	assert.NotEmpty(t, errBody.Error.Data.QualifiedID, "mismatch payload must echo qualified_id")
}

// TestCreate_IdempotencyMismatchOnPriorityChange verifies that reusing the
// same Idempotency-Key with a different priority is a 409 mismatch — priority
// is part of the request identity, so a creator who keys on a body+priority
// pair gets the right surface when a later request shifts priority.
func TestCreate_IdempotencyMismatchOnPriorityChange(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	path := issuesURL(pid)

	first := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "Kprio"},
		map[string]any{"actor": "agent-1", "title": "issue", "body": "body", "priority": 1})
	requireOK(t, first)

	second := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "Kprio"},
		map[string]any{"actor": "agent-1", "title": "issue", "body": "body", "priority": 2})
	require.Equal(t, 409, second.status, string(second.body))
	assert.Contains(t, string(second.body), `"code":"idempotency_mismatch"`)

	// Re-sending with the same priority reuses the original issue.
	third := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "Kprio"},
		map[string]any{"actor": "agent-1", "title": "issue", "body": "body", "priority": 1})
	requireOK(t, third)
	var thirdOut struct {
		Reused bool `json:"reused"`
	}
	require.NoError(t, json.Unmarshal(third.body, &thirdOut))
	assert.True(t, thirdOut.Reused, "same key + same priority should reuse")
}

// TestCreate_PriorityValidatedBeforeIdempotency verifies that an out-of-range
// priority surfaces as a 400 even when an Idempotency-Key matches a prior
// issue. The validate-before-lookup ordering keeps the API contract honest:
// invalid input is never silently absorbed by a reuse path.
func TestCreate_PriorityValidatedBeforeIdempotency(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	path := issuesURL(pid)

	first := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "Kbad"},
		map[string]any{"actor": "agent-1", "title": "issue", "body": "body"})
	requireOK(t, first)

	bad := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "Kbad"},
		map[string]any{"actor": "agent-1", "title": "issue", "body": "body", "priority": 9})
	require.Equal(t, 400, bad.status, string(bad.body))
	assert.Contains(t, string(bad.body), "priority must be between 0 and 4")
}

// TestCreate_IdempotencyDeletedIs409 verifies the §3.6 deleted-issue branch:
// when the idempotent-matched issue has been soft-deleted, retrying with the
// same key yields 409 idempotency_deleted with a restore hint.
func TestCreate_IdempotencyDeletedIs409(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	path := issuesURL(pid)

	first := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "K-DEL"},
		map[string]any{"actor": "agent-1", "title": "soft delete me", "body": "details"})
	requireOK(t, first)
	var firstResp struct {
		Issue struct{ ID int64 } `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(first.body, &firstResp))

	// Soft-delete the issue at the DB layer (Task 5 ships SoftDeleteIssue;
	// the daemon wraps it in Task 10. We can call DB directly because the
	// test server shares the same *sqlitestore.Store as the handler.)
	_, _, _, err := h.DB().SoftDeleteIssue(t.Context(), firstResp.Issue.ID, "agent-1")
	require.NoError(t, err)

	second := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "K-DEL"},
		map[string]any{"actor": "agent-1", "title": "soft delete me", "body": "details"})
	require.Equal(t, 409, second.status, string(second.body))
	assert.Contains(t, string(second.body), `"code":"idempotency_deleted"`)
	// Look up the original issue's short_id to pin the hint format.
	deletedIssue, err := h.DB().IssueByID(context.Background(), firstResp.Issue.ID)
	require.NoError(t, err)
	assert.Contains(t, string(second.body), "kata restore "+deletedIssue.ShortID,
		"hint must point at the restore command")
}

// TestCreate_LookalikeSoftBlock verifies that a near-identical second create
// (same title+body) without Idempotency-Key and without force_new is rejected
// as 409 duplicate_candidates.
func TestCreate_LookalikeSoftBlock(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	path := issuesURL(pid)
	body := map[string]any{"actor": "agent-1", "title": "fix login crash", "body": "stack trace here"}

	first := postWithHeader(t, ts, path, nil, body)
	requireOK(t, first)

	second := postWithHeader(t, ts, path, nil, body)
	require.Equal(t, 409, second.status, string(second.body))
	assert.Contains(t, string(second.body), `"code":"duplicate_candidates"`)
	assert.Contains(t, string(second.body), `"candidates"`)
}

// TestCreate_ForceNewBypassesLookalike verifies that force_new=true on a body
// that would otherwise trip the look-alike check creates a new issue (200).
func TestCreate_ForceNewBypassesLookalike(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	path := issuesURL(pid)

	first := postWithHeader(t, ts, path, nil,
		map[string]any{"actor": "agent-1", "title": "fix login crash", "body": "stack trace here"})
	requireOK(t, first)
	var firstOut struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(first.body, &firstOut))

	second := postWithHeader(t, ts, path, nil,
		map[string]any{"actor": "agent-1", "title": "fix login crash", "body": "stack trace here", "force_new": true})
	requireOK(t, second)
	var out struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(second.body, &out))
	assert.NotEmpty(t, out.Issue.ShortID, "force_new response must carry a short_id")
	assert.NotEqual(t, firstOut.Issue.ShortID, out.Issue.ShortID,
		"force_new must yield a new issue, not reuse")
}

// TestCreate_IdempotencyWinsOverForceNew verifies the spec §3.7 ordering: an
// idempotent match returns reuse even when force_new=true is also set.
func TestCreate_IdempotencyWinsOverForceNew(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	path := issuesURL(pid)
	body := map[string]any{"actor": "agent-1", "title": "fix login crash", "body": "stack trace here"}

	first := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "K1"}, body)
	requireOK(t, first)
	var firstOut struct {
		Issue struct{ Number int64 } `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(first.body, &firstOut))

	bodyForceNew := map[string]any{
		"actor": "agent-1", "title": "fix login crash", "body": "stack trace here", "force_new": true,
	}
	second := postWithHeader(t, ts, path, map[string]string{"Idempotency-Key": "K1"}, bodyForceNew)
	requireOK(t, second)
	var secondOut struct {
		Issue  struct{ Number int64 } `json:"issue"`
		Reused bool                   `json:"reused"`
	}
	require.NoError(t, json.Unmarshal(second.body, &secondOut))
	assert.Equal(t, firstOut.Issue.Number, secondOut.Issue.Number, "idempotency wins: same number returned")
	assert.True(t, secondOut.Reused)
}

// TestListIssues_HydratesLabels verifies the Plan 8 commit 5b
// contract: GET /api/v1/projects/{id}/issues returns each issue with
// its attached labels (sorted alphabetically), so the TUI list view
// can render label chips without an extra fetch per row.
func TestListIssues_HydratesLabels(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	first := createIssueViaHTTP(t, env, pid, "first")
	second := createIssueViaHTTP(t, env, pid, "second")
	postLabel(t, env, pid, first, "prio-1")
	postLabel(t, env, pid, first, "bug")
	postLabel(t, env, pid, second, "enhancement")

	firstShort := refForIssue(t, env, first)
	secondShort := refForIssue(t, env, second)
	var out struct {
		Issues []struct {
			ShortID string   `json:"short_id"`
			Labels  []string `json:"labels"`
		} `json:"issues"`
	}
	envGetJSON(t, env, projectPath(pid)+"/issues", &out)
	require.Len(t, out.Issues, 2)
	byShort := map[string][]string{}
	for _, iss := range out.Issues {
		byShort[iss.ShortID] = iss.Labels
	}
	assert.Equal(t, []string{"bug", "prio-1"}, byShort[firstShort])
	assert.Equal(t, []string{"enhancement"}, byShort[secondShort])
}

func TestListIssues_IncludesHierarchyMetadata(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent")
	child := createIssueViaHTTP(t, env, pid, "child")
	postLink(t, env, pid, child, "parent", parent)
	parentShort := refForIssue(t, env, parent)
	childShort := refForIssue(t, env, child)

	var out struct {
		Issues []struct {
			ShortID       string  `json:"short_id"`
			ParentShortID *string `json:"parent_short_id"`
			ChildCounts   *struct {
				Open  int `json:"open"`
				Total int `json:"total"`
			} `json:"child_counts"`
		} `json:"issues"`
	}
	envGetJSON(t, env, projectPath(pid)+"/issues", &out)
	require.Len(t, out.Issues, 2)
	byShort := map[string]struct {
		ParentShortID *string
		ChildCounts   *struct {
			Open  int `json:"open"`
			Total int `json:"total"`
		}
	}{}
	for _, iss := range out.Issues {
		byShort[iss.ShortID] = struct {
			ParentShortID *string
			ChildCounts   *struct {
				Open  int `json:"open"`
				Total int `json:"total"`
			}
		}{ParentShortID: iss.ParentShortID, ChildCounts: iss.ChildCounts}
	}
	require.NotNil(t, byShort[parentShort].ChildCounts)
	assert.Equal(t, 1, byShort[parentShort].ChildCounts.Open)
	assert.Equal(t, 1, byShort[parentShort].ChildCounts.Total)
	require.NotNil(t, byShort[childShort].ParentShortID)
	assert.Equal(t, parentShort, *byShort[childShort].ParentShortID)
}

func TestListIssues_IncludesBlockerMetadata(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	blocker := createIssueViaHTTP(t, env, pid, "blocker")
	blocked := createIssueViaHTTP(t, env, pid, "blocked")
	postLink(t, env, pid, blocker, "blocks", blocked)
	blockerShort := refForIssue(t, env, blocker)
	blockedShort := refForIssue(t, env, blocked)

	var out struct {
		Issues []struct {
			ShortID string         `json:"short_id"`
			Blocks  []linkPeerTest `json:"blocks,omitempty"`
		} `json:"issues"`
	}
	envGetJSON(t, env, projectPath(pid)+"/issues", &out)
	byShort := map[string][]linkPeerTest{}
	for _, iss := range out.Issues {
		byShort[iss.ShortID] = iss.Blocks
	}
	require.Len(t, byShort[blockerShort], 1)
	assert.Equal(t, blockedShort, byShort[blockerShort][0].ShortID)
	assert.Empty(t, byShort[blockedShort])
}

// TestListAllIssues_AcrossProjects pins #22's wire contract: GET /api/v1/issues
// with no project_id returns issues from every project, hydrating labels
// per-issue across project boundaries.
func TestListAllIssues_AcrossProjects(t *testing.T) {
	env := testenv.New(t)
	pidA := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/proj-a.git")
	pidB := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/proj-b.git")
	createIssueViaHTTP(t, env, pidA, "alpha-1")
	createIssueViaHTTP(t, env, pidB, "beta-1")
	createIssueViaHTTP(t, env, pidA, "alpha-2")

	var out struct {
		Issues []struct {
			ProjectID int64  `json:"project_id"`
			Title     string `json:"title"`
		} `json:"issues"`
	}
	envGetJSON(t, env, "/api/v1/issues", &out)
	require.Len(t, out.Issues, 3)
	projectIDs := map[int64]int{}
	for _, iss := range out.Issues {
		projectIDs[iss.ProjectID]++
	}
	assert.Equal(t, 2, projectIDs[pidA], "two issues from project A")
	assert.Equal(t, 1, projectIDs[pidB], "one issue from project B")
}

// TestListAllIssues_ProjectFilter pins the optional ?project_id= query: the
// cross-project endpoint can also serve as a single-project list when needed.
func TestListAllIssues_ProjectFilter(t *testing.T) {
	env := testenv.New(t)
	pidA := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/proj-a.git")
	pidB := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/proj-b.git")
	createIssueViaHTTP(t, env, pidA, "alpha-1")
	createIssueViaHTTP(t, env, pidB, "beta-1")

	var out struct {
		Issues []struct {
			ProjectID int64  `json:"project_id"`
			Title     string `json:"title"`
		} `json:"issues"`
	}
	envGetJSON(t, env, "/api/v1/issues?project_id="+strconv.FormatInt(pidB, 10), &out)
	require.Len(t, out.Issues, 1)
	assert.Equal(t, pidB, out.Issues[0].ProjectID)
	assert.Equal(t, "beta-1", out.Issues[0].Title)
}

// TestListAllIssues_ProjectNotFound pins the 404 path: a project_id that
// doesn't exist surfaces as project_not_found, matching the per-project
// endpoint's contract for invalid IDs.
func TestListAllIssues_ProjectNotFound(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envGetRaw(t, env, "/api/v1/issues?project_id=9999")
	assertAPIError(t, resp.StatusCode, bs, http.StatusNotFound, "project_not_found")
}

// TestListAllIssues_RejectsRemovedViewParams guards against silent semantic
// regressions for stale clients still sending the named-view query params or
// the X-Kata-Client-TZ header that were removed when server-computed views
// moved client-side. Returning 200 with the unfiltered feed for ?view=logbook
// would mislead a stale client; 400 makes the breakage obvious.
func TestListAllIssues_RejectsRemovedViewParams(t *testing.T) {
	env := testenv.New(t)
	for _, q := range []string{
		"/api/v1/issues?view=today",
		"/api/v1/issues?view=logbook",
		"/api/v1/issues?area=Personal",
		"/api/v1/issues?offset=10",
		// offset=0 must reject too — presence matters, value does not.
		"/api/v1/issues?offset=0",
	} {
		t.Run(q, func(t *testing.T) {
			resp, bs := envGetRaw(t, env, q)
			assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "removed_param")
		})
	}
}

// TestListAllIssues_RejectsRemovedClientTZHeader covers the header arm of the
// removed_param guard so a stale client still sending the timezone hint is
// surfaced as a clear 400 rather than silently honored.
func TestListAllIssues_RejectsRemovedClientTZHeader(t *testing.T) {
	env := testenv.New(t)
	req, err := http.NewRequest(http.MethodGet, env.URL+"/api/v1/issues", nil)
	require.NoError(t, err)
	req.Header.Set("X-Kata-Client-TZ", "America/New_York")
	resp, err := env.HTTP.Do(req) //nolint:gosec // test request to loopback URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, _ := io.ReadAll(resp.Body)
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "removed_param")
}

// TestListAllIssues_HydratesLabelsAcrossProjects pins that labels attach
// correctly to rows from different projects — the hydration helper groups by
// project_id internally so labels stay scoped to the right issue.
func TestListAllIssues_HydratesLabelsAcrossProjects(t *testing.T) {
	env := testenv.New(t)
	pidA := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/proj-a.git")
	pidB := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/proj-b.git")
	a1 := createIssueViaHTTP(t, env, pidA, "alpha-1")
	b1 := createIssueViaHTTP(t, env, pidB, "beta-1")
	postLabel(t, env, pidA, a1, "bug")
	postLabel(t, env, pidB, b1, "enhancement")
	a1Short := refForIssue(t, env, a1)
	b1Short := refForIssue(t, env, b1)

	var out struct {
		Issues []struct {
			ProjectID int64    `json:"project_id"`
			ShortID   string   `json:"short_id"`
			Labels    []string `json:"labels"`
		} `json:"issues"`
	}
	envGetJSON(t, env, "/api/v1/issues", &out)
	labelsByKey := map[string][]string{}
	for _, iss := range out.Issues {
		key := strconv.FormatInt(iss.ProjectID, 10) + "/" + iss.ShortID
		labelsByKey[key] = iss.Labels
	}
	assert.Equal(t, []string{"bug"},
		labelsByKey[strconv.FormatInt(pidA, 10)+"/"+a1Short])
	assert.Equal(t, []string{"enhancement"},
		labelsByKey[strconv.FormatInt(pidB, 10)+"/"+b1Short])
}

func TestShowIssue_IncludesLinksAndLabels(t *testing.T) {
	env := testenv.New(t)
	pid, parent, child := setupTwoIssues(t, env)
	postLabel(t, env, pid, child, "bug")
	postLink(t, env, pid, child, "parent", parent)
	parentShort := refForIssue(t, env, parent)
	childShort := refForIssue(t, env, child)

	var out struct {
		Links []struct {
			Type string       `json:"type"`
			From linkPeerTest `json:"from"`
			To   linkPeerTest `json:"to"`
		} `json:"links"`
		Labels []struct {
			Label string `json:"label"`
		} `json:"labels"`
	}
	envGetJSON(t, env, issuePath(pid, child, ""), &out)
	require.Len(t, out.Links, 1)
	assert.Equal(t, "parent", out.Links[0].Type)
	assert.Equal(t, childShort, out.Links[0].From.ShortID)
	assert.Equal(t, parentShort, out.Links[0].To.ShortID)
	require.Len(t, out.Labels, 1)
	assert.Equal(t, "bug", out.Labels[0].Label)
}

func TestShowIssue_IncludesParentAndChildren(t *testing.T) {
	env := testenv.New(t)
	pid := initWorkspaceViaHTTP(t, env, "https://github.com/wesm/kata.git")
	parent := createIssueViaHTTP(t, env, pid, "parent")
	child := createIssueViaHTTP(t, env, pid, "child")
	grandchild := createIssueViaHTTP(t, env, pid, "grandchild")
	greatGrandchild := createIssueViaHTTP(t, env, pid, "great grandchild")
	postLink(t, env, pid, child, "parent", parent)
	postLink(t, env, pid, grandchild, "parent", child)
	postLink(t, env, pid, greatGrandchild, "parent", grandchild)
	postLabel(t, env, pid, grandchild, "bug")
	parentShort := refForIssue(t, env, parent)
	grandchildShort := refForIssue(t, env, grandchild)

	var out struct {
		Parent *struct {
			ShortID string `json:"short_id"`
			Title   string `json:"title"`
			Status  string `json:"status"`
		} `json:"parent"`
		Children []struct {
			ShortID     string   `json:"short_id"`
			Labels      []string `json:"labels"`
			ChildCounts *struct {
				Open  int `json:"open"`
				Total int `json:"total"`
			} `json:"child_counts"`
		} `json:"children"`
	}
	envGetJSON(t, env, issuePath(pid, child, ""), &out)
	require.NotNil(t, out.Parent)
	assert.Equal(t, parentShort, out.Parent.ShortID)
	assert.Equal(t, "parent", out.Parent.Title)
	assert.Equal(t, "open", out.Parent.Status)
	require.Len(t, out.Children, 1)
	assert.Equal(t, grandchildShort, out.Children[0].ShortID)
	assert.Equal(t, []string{"bug"}, out.Children[0].Labels)
	require.NotNil(t, out.Children[0].ChildCounts)
	assert.Equal(t, 1, out.Children[0].ChildCounts.Open)
	assert.Equal(t, 1, out.Children[0].ChildCounts.Total)
}
