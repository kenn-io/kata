package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/testenv"
)

func revETag(rev int64) string {
	return fmt.Sprintf(`"rev-%d"`, rev)
}

func issueRoute(projectID int64, ref, suffix string) string {
	if suffix == "" {
		return fmt.Sprintf("/api/v1/projects/%d/issues/%s", projectID, ref)
	}
	return fmt.Sprintf("/api/v1/projects/%d/issues/%s/%s", projectID, ref, suffix)
}

func doRawWithExplicitHeader(
	t *testing.T,
	baseURL, method, path string,
	body any,
	headers map[string]string,
) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, baseURL+path, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		// Use direct map assignment so an empty string value is actually sent
		// as a present-but-empty HTTP header rather than being omitted.
		req.Header[http.CanonicalHeaderKey(k)] = []string{v}
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // httptest URL is test-controlled
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, raw
}

func readIssueRevision(t *testing.T, store db.Storage, issueID int64) db.Issue {
	t.Helper()
	iss, err := store.IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	return iss
}

func TestIssueWriteRoutesHonorOptionalIfMatch(t *testing.T) {
	type routeCase struct {
		name      string
		setup     func(t *testing.T, env *testenv.Env, project db.Project) (issue db.Issue, method, path string, body any)
		assertRan func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, raw []byte)
	}

	cases := []routeCase{
		{
			name: "edit priority",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "edit me", Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPatch, issueRoute(project.ID, iss.ShortID, ""),
					map[string]any{"actor": "worker", "set_priority": 1}
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				require.NotNil(t, after.Priority)
				assert.Equal(t, int64(1), *after.Priority)
				assert.Equal(t, beforeRev+1, after.Revision)
			},
		},
		{
			name: "reopen",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "reopen me", Author: "tester",
				})
				require.NoError(t, err)
				closed, _, changed, err := env.DB.CloseIssue(t.Context(), iss.ID, "done", "tester", "done", nil)
				require.NoError(t, err)
				require.True(t, changed)
				return closed, http.MethodPost, issueRoute(project.ID, closed.ShortID, "actions/reopen"),
					map[string]any{"actor": "worker"}
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				assert.Equal(t, "open", after.Status)
				assert.Equal(t, beforeRev+1, after.Revision)
			},
		},
		{
			name: "create link",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "source", Author: "tester",
				})
				require.NoError(t, err)
				peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "target", Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPost, issueRoute(project.ID, iss.ShortID, "links"),
					map[string]any{"actor": "worker", "type": "blocks", "to_ref": peer.ShortID}
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				assert.Equal(t, beforeRev+1, after.Revision)
				links, err := env.DB.LinksByIssue(t.Context(), issueID)
				require.NoError(t, err)
				assert.Len(t, links, 1)
			},
		},
		{
			name: "delete link",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "source", Author: "tester",
				})
				require.NoError(t, err)
				peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "target", Author: "tester",
				})
				require.NoError(t, err)
				link, _, err := env.DB.CreateLinkAndEvent(t.Context(),
					db.CreateLinkParams{FromIssueID: iss.ID, ToIssueID: peer.ID, Type: "blocks", Author: "tester"},
					db.LinkEventParams{
						EventType: "issue.linked", EventIssueID: iss.ID,
						FromShortID: iss.ShortID, FromUID: iss.UID,
						ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "tester",
					})
				require.NoError(t, err)
				current := readIssueRevision(t, env.DB, iss.ID)
				return current, http.MethodDelete,
					fmt.Sprintf("%s?actor=worker", issueRoute(project.ID, current.ShortID, fmt.Sprintf("links/%d", link.ID))),
					nil
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				assert.Equal(t, beforeRev+1, after.Revision)
				links, err := env.DB.LinksByIssue(t.Context(), issueID)
				require.NoError(t, err)
				assert.Empty(t, links)
			},
		},
		{
			name: "add label",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "label me", Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPost, issueRoute(project.ID, iss.ShortID, "labels"),
					map[string]any{"actor": "worker", "label": "review-ready"}
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				assert.Equal(t, beforeRev+1, after.Revision)
				assert.Equal(t, []string{"review-ready"}, issueLabelNames(t, env, issueID))
			},
		},
		{
			name: "update labels batch",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "batch label me", Author: "tester",
				})
				require.NoError(t, err)
				_, _, err = env.DB.AddLabelAndEvent(t.Context(), iss.ID, db.LabelEventParams{
					EventType: "issue.labeled", Label: "old", Actor: "tester",
				})
				require.NoError(t, err)
				current := readIssueRevision(t, env.DB, iss.ID)
				return current, http.MethodPatch, issueRoute(project.ID, current.ShortID, "labels"),
					map[string]any{"actor": "worker", "add": []string{"new"}, "remove": []string{"old"}}
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				assert.Equal(t, beforeRev+1, after.Revision)
				assert.Equal(t, []string{"new"}, issueLabelNames(t, env, issueID))
			},
		},
		{
			name: "remove label",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "unlabel me", Author: "tester",
				})
				require.NoError(t, err)
				_, _, err = env.DB.AddLabelAndEvent(t.Context(), iss.ID, db.LabelEventParams{
					EventType: "issue.labeled", Label: "review-ready", Actor: "tester",
				})
				require.NoError(t, err)
				current := readIssueRevision(t, env.DB, iss.ID)
				return current, http.MethodDelete,
					fmt.Sprintf("%s?actor=worker", issueRoute(project.ID, current.ShortID, "labels/review-ready")),
					nil
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				assert.Equal(t, beforeRev+1, after.Revision)
				assert.Empty(t, issueLabelNames(t, env, issueID))
			},
		},
		{
			name: "assign",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "assign me", Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPost, issueRoute(project.ID, iss.ShortID, "actions/assign"),
					map[string]any{"actor": "worker", "owner": "alice"}
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				require.NotNil(t, after.Owner)
				assert.Equal(t, "alice", *after.Owner)
				assert.Equal(t, beforeRev+1, after.Revision)
			},
		},
		{
			name: "unassign",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				owner := "alice"
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "unassign me", Author: "tester", Owner: &owner,
				})
				require.NoError(t, err)
				return iss, http.MethodPost, issueRoute(project.ID, iss.ShortID, "actions/unassign"),
					map[string]any{"actor": "worker"}
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				assert.Nil(t, after.Owner)
				assert.Equal(t, beforeRev+1, after.Revision)
			},
		},
		{
			name: "priority action",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "prioritize me", Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPost, issueRoute(project.ID, iss.ShortID, "actions/priority"),
					map[string]any{"actor": "worker", "priority": 2}
			},
			assertRan: func(t *testing.T, env *testenv.Env, issueID int64, beforeRev int64, _ []byte) {
				after := readIssueRevision(t, env.DB, issueID)
				require.NotNil(t, after.Priority)
				assert.Equal(t, int64(2), *after.Priority)
				assert.Equal(t, beforeRev+1, after.Revision)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("present empty If-Match returns 400 and writes nothing", func(t *testing.T) {
				env := testenv.New(t)
				project := seedProject(t, env, "shared-project")
				iss, method, path, body := tc.setup(t, env, project)
				eventsBefore, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)

				resp, raw := doRawWithExplicitHeader(t, env.URL, method, path, body, map[string]string{"If-Match": ""})
				assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")

				after := readIssueRevision(t, env.DB, iss.ID)
				assert.Equal(t, iss.Revision, after.Revision)
				eventsAfter, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)
				assert.Len(t, eventsAfter, len(eventsBefore))
			})

			t.Run("malformed If-Match returns 400 and writes nothing", func(t *testing.T) {
				env := testenv.New(t)
				project := seedProject(t, env, "shared-project")
				iss, method, path, body := tc.setup(t, env, project)

				resp, raw := doRawWithExplicitHeader(t, env.URL, method, path, body, map[string]string{"If-Match": `"bogus"`})
				assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")

				after := readIssueRevision(t, env.DB, iss.ID)
				assert.Equal(t, iss.Revision, after.Revision)
			})

			t.Run("stale If-Match returns 412 revision_conflict and writes nothing", func(t *testing.T) {
				env := testenv.New(t)
				project := seedProject(t, env, "shared-project")
				iss, method, path, body := tc.setup(t, env, project)
				eventsBefore, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)

				resp, raw := doRawWithExplicitHeader(t, env.URL, method, path, body, map[string]string{"If-Match": revETag(iss.Revision + 99)})
				assertAPIError(t, resp.StatusCode, raw, http.StatusPreconditionFailed, "revision_conflict")
				assert.Contains(t, string(raw), fmt.Sprintf("issue revision is %d", iss.Revision))

				after := readIssueRevision(t, env.DB, iss.ID)
				assert.Equal(t, iss.Revision, after.Revision)
				eventsAfter, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)
				assert.Len(t, eventsAfter, len(eventsBefore))
			})

			t.Run("matching If-Match succeeds and advances revision", func(t *testing.T) {
				env := testenv.New(t)
				project := seedProject(t, env, "shared-project")
				iss, method, path, body := tc.setup(t, env, project)

				resp, raw := doRawWithExplicitHeader(t, env.URL, method, path, body, map[string]string{"If-Match": revETag(iss.Revision)})
				require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
				tc.assertRan(t, env, iss.ID, iss.Revision, raw)
			})

			t.Run("absent If-Match succeeds unconditionally", func(t *testing.T) {
				env := testenv.New(t)
				project := seedProject(t, env, "shared-project")
				iss, method, path, body := tc.setup(t, env, project)

				resp, raw := doRawWithExplicitHeader(t, env.URL, method, path, body, nil)
				require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
				tc.assertRan(t, env, iss.ID, iss.Revision, raw)
			})
		})
	}
}

func TestLinkMutationsAdvanceBothEndpointsAndBlockStalePeerWrites(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "shared-project")
	child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "child", Author: "tester",
	})
	require.NoError(t, err)
	parentA, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "parent-a", Author: "tester",
	})
	require.NoError(t, err)
	parentB, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "parent-b", Author: "tester",
	})
	require.NoError(t, err)

	parentARev0 := parentA.Revision
	parentBRev0 := parentB.Revision

	// POST /links child -> parentA advances both child and parentA.
	resp, raw := doRawWithExplicitHeader(t, env.URL, http.MethodPost,
		issueRoute(project.ID, child.ShortID, "links"),
		map[string]any{"actor": "worker", "type": "parent", "to_ref": parentA.ShortID},
		map[string]string{"If-Match": revETag(child.Revision)})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	childAfterLink := readIssueRevision(t, env.DB, child.ID)
	parentAAfterLink := readIssueRevision(t, env.DB, parentA.ID)
	assert.Equal(t, child.Revision+1, childAfterLink.Revision)
	assert.Equal(t, parentARev0+1, parentAAfterLink.Revision)

	// A stale If-Match on parentA (captured before child linked to it) must be rejected.
	resp, raw = doRawWithExplicitHeader(t, env.URL, http.MethodPatch,
		issueRoute(project.ID, parentA.ShortID, ""),
		map[string]any{"actor": "worker", "set_priority": 1},
		map[string]string{"If-Match": revETag(parentARev0)})
	assertAPIError(t, resp.StatusCode, raw, http.StatusPreconditionFailed, "revision_conflict")

	// POST /links replace=true swaps parentA -> parentB, advancing child, parentA, and parentB.
	resp, raw = doRawWithExplicitHeader(t, env.URL, http.MethodPost,
		issueRoute(project.ID, child.ShortID, "links"),
		map[string]any{"actor": "worker", "type": "parent", "to_ref": parentB.ShortID, "replace": true},
		map[string]string{"If-Match": revETag(childAfterLink.Revision)})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	var replaceOut struct {
		Link struct {
			ID int64 `json:"id"`
		} `json:"link"`
	}
	require.NoError(t, json.Unmarshal(raw, &replaceOut))

	childAfterReplace := readIssueRevision(t, env.DB, child.ID)
	parentAAfterReplace := readIssueRevision(t, env.DB, parentA.ID)
	parentBAfterReplace := readIssueRevision(t, env.DB, parentB.ID)
	assert.Equal(t, childAfterLink.Revision+1, childAfterReplace.Revision)
	assert.Equal(t, parentAAfterLink.Revision+1, parentAAfterReplace.Revision)
	assert.Equal(t, parentBRev0+1, parentBAfterReplace.Revision)

	// DELETE /links/{id} removes child -> parentB, advancing both child and parentB.
	resp, raw = doRawWithExplicitHeader(t, env.URL, http.MethodDelete,
		fmt.Sprintf("%s?actor=worker", issueRoute(project.ID, child.ShortID, fmt.Sprintf("links/%d", replaceOut.Link.ID))),
		nil, map[string]string{"If-Match": revETag(childAfterReplace.Revision)})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	childAfterDelete := readIssueRevision(t, env.DB, child.ID)
	parentBAfterDelete := readIssueRevision(t, env.DB, parentB.ID)
	assert.Equal(t, childAfterReplace.Revision+1, childAfterDelete.Revision)
	assert.Equal(t, parentBAfterReplace.Revision+1, parentBAfterDelete.Revision)
}

func TestEditIssuePreservesExpectedContentRevisionAlongsideIfMatch(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "shared-project")
	iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "original", Body: "body-0", Author: "tester",
	})
	require.NoError(t, err)

	// Title-only edit with matching If-Match and expected_content_revision advances
	// content_revision while leaving revision unchanged.
	resp, raw := doRawWithExplicitHeader(t, env.URL, http.MethodPatch,
		issueRoute(project.ID, iss.ShortID, ""),
		map[string]any{
			"actor":                     "worker",
			"title":                     "updated-title",
			"expected_content_revision": iss.ContentRevision,
		},
		map[string]string{"If-Match": revETag(iss.Revision)})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	afterTitle := readIssueRevision(t, env.DB, iss.ID)
	assert.Equal(t, iss.Revision, afterTitle.Revision)
	assert.Equal(t, iss.ContentRevision+1, afterTitle.ContentRevision)

	// Fresh If-Match with stale expected_content_revision fails with 412 and writes nothing.
	resp, raw = doRawWithExplicitHeader(t, env.URL, http.MethodPatch,
		issueRoute(project.ID, iss.ShortID, ""),
		map[string]any{
			"actor":                     "worker",
			"set_priority":              1,
			"expected_content_revision": iss.ContentRevision,
		},
		map[string]string{"If-Match": revETag(afterTitle.Revision)})
	assertAPIError(t, resp.StatusCode, raw, http.StatusPreconditionFailed, "revision_conflict")
	assert.Nil(t, readIssueRevision(t, env.DB, iss.ID).Priority)

	// Label change advances revision without changing content_revision.
	resp, raw = doRawWithExplicitHeader(t, env.URL, http.MethodPost,
		issueRoute(project.ID, iss.ShortID, "labels"),
		map[string]any{"actor": "worker", "label": "review-ready"},
		map[string]string{"If-Match": revETag(afterTitle.Revision)})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	afterLabel := readIssueRevision(t, env.DB, iss.ID)
	assert.Equal(t, afterTitle.Revision+1, afterLabel.Revision)
	assert.Equal(t, afterTitle.ContentRevision, afterLabel.ContentRevision)

	// Stale If-Match with fresh expected_content_revision fails with 412 and writes nothing.
	resp, raw = doRawWithExplicitHeader(t, env.URL, http.MethodPatch,
		issueRoute(project.ID, iss.ShortID, ""),
		map[string]any{
			"actor":                     "worker",
			"body":                      "must-not-write",
			"expected_content_revision": afterLabel.ContentRevision,
		},
		map[string]string{"If-Match": revETag(afterTitle.Revision)})
	assertAPIError(t, resp.StatusCode, raw, http.StatusPreconditionFailed, "revision_conflict")
	assert.Equal(t, "body-0", readIssueRevision(t, env.DB, iss.ID).Body)
}

func TestIssueWriteRoutesNoOpIfMatch(t *testing.T) {
	type noopCase struct {
		name  string
		setup func(t *testing.T, env *testenv.Env, project db.Project) (issue db.Issue, method, path string, body any)
	}

	cases := []noopCase{
		{
			name: "edit priority unchanged",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				p := int64(2)
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "same prio", Priority: &p, Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPatch, issueRoute(project.ID, iss.ShortID, ""),
					map[string]any{"actor": "worker", "set_priority": 2}
			},
		},
		{
			name: "priority action unchanged",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				p := int64(2)
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "same prio action", Priority: &p, Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPost, issueRoute(project.ID, iss.ShortID, "actions/priority"),
					map[string]any{"actor": "worker", "priority": 2}
			},
		},
		{
			name: "reopen already open",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "already open", Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPost, issueRoute(project.ID, iss.ShortID, "actions/reopen"),
					map[string]any{"actor": "worker"}
			},
		},
		{
			name: "add existing label",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "labeled", Author: "tester",
				})
				require.NoError(t, err)
				_, _, err = env.DB.AddLabelAndEvent(t.Context(), iss.ID, db.LabelEventParams{
					EventType: "issue.labeled", Label: "existing", Actor: "tester",
				})
				require.NoError(t, err)
				current := readIssueRevision(t, env.DB, iss.ID)
				return current, http.MethodPost, issueRoute(project.ID, current.ShortID, "labels"),
					map[string]any{"actor": "worker", "label": "existing"}
			},
		},
		{
			name: "batch labels no-op",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "labeled", Author: "tester",
				})
				require.NoError(t, err)
				_, _, err = env.DB.AddLabelAndEvent(t.Context(), iss.ID, db.LabelEventParams{
					EventType: "issue.labeled", Label: "existing", Actor: "tester",
				})
				require.NoError(t, err)
				current := readIssueRevision(t, env.DB, iss.ID)
				return current, http.MethodPatch, issueRoute(project.ID, current.ShortID, "labels"),
					map[string]any{"actor": "worker", "add": []string{"existing"}, "remove": []string{"absent"}}
			},
		},
		{
			name: "remove absent label",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "unlabeled", Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodDelete,
					fmt.Sprintf("%s?actor=worker", issueRoute(project.ID, iss.ShortID, "labels/absent")),
					nil
			},
		},
		{
			name: "create duplicate link",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "src", Author: "tester",
				})
				require.NoError(t, err)
				peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "dst", Author: "tester",
				})
				require.NoError(t, err)
				_, _, err = env.DB.CreateLinkAndEvent(t.Context(),
					db.CreateLinkParams{FromIssueID: iss.ID, ToIssueID: peer.ID, Type: "blocks", Author: "tester"},
					db.LinkEventParams{
						EventType: "issue.linked", EventIssueID: iss.ID,
						FromShortID: iss.ShortID, FromUID: iss.UID,
						ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "tester",
					})
				require.NoError(t, err)
				current := readIssueRevision(t, env.DB, iss.ID)
				return current, http.MethodPost, issueRoute(project.ID, current.ShortID, "links"),
					map[string]any{"actor": "worker", "type": "blocks", "to_ref": peer.ShortID}
			},
		},
		{
			name: "assign same owner",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				owner := "alice"
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "owned", Owner: &owner, Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPost, issueRoute(project.ID, iss.ShortID, "actions/assign"),
					map[string]any{"actor": "worker", "owner": "alice"}
			},
		},
		{
			name: "unassign already unowned",
			setup: func(t *testing.T, env *testenv.Env, project db.Project) (db.Issue, string, string, any) {
				iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "unowned", Author: "tester",
				})
				require.NoError(t, err)
				return iss, http.MethodPost, issueRoute(project.ID, iss.ShortID, "actions/unassign"),
					map[string]any{"actor": "worker"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			project := seedProject(t, env, "shared-project")
			iss, method, path, body := tc.setup(t, env, project)

			// Stale If-Match must still be rejected with 412 even when the write would be a no-op.
			resp, raw := doRawWithExplicitHeader(t, env.URL, method, path, body, map[string]string{"If-Match": revETag(iss.Revision + 5)})
			assertAPIError(t, resp.StatusCode, raw, http.StatusPreconditionFailed, "revision_conflict")

			// Matching If-Match succeeds without bumping revision.
			resp, raw = doRawWithExplicitHeader(t, env.URL, method, path, body, map[string]string{"If-Match": revETag(iss.Revision)})
			require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
			after := readIssueRevision(t, env.DB, iss.ID)
			assert.Equal(t, iss.Revision, after.Revision, "no-op write must not bump revision")
		})
	}
}

func runConcurrentPriorityEditCheck(t *testing.T, baseURL string, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	prio3 := int64(3)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "race priority", Priority: &prio3, Author: "automation",
	})
	require.NoError(t, err)

	path := issueRoute(project.ID, issue.ShortID, "")
	etag := revETag(issue.Revision)
	type outcome struct {
		priority int64
		status   int
		body     []byte
		err      error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, targetPriority := range []int64{1, 2} {
		go func(p int64) {
			ready.Done()
			<-start
			payload, mErr := json.Marshal(map[string]any{"actor": "automation", "set_priority": p})
			if mErr != nil {
				results <- outcome{priority: p, err: mErr}
				return
			}
			req, rErr := http.NewRequestWithContext(ctx, http.MethodPatch, baseURL+path, bytes.NewReader(payload))
			if rErr != nil {
				results <- outcome{priority: p, err: rErr}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", etag)
			resp, doErr := http.DefaultClient.Do(req) //nolint:gosec // httptest URL is test-controlled
			if doErr != nil {
				results <- outcome{priority: p, err: doErr}
				return
			}
			raw, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				results <- outcome{priority: p, err: readErr}
				return
			}
			if closeErr != nil {
				results <- outcome{priority: p, err: closeErr}
				return
			}
			results <- outcome{priority: p, status: resp.StatusCode, body: raw}
		}(targetPriority)
	}
	ready.Wait()
	close(start)

	var winnerPriority int64
	wins, conflicts := 0, 0
	for range 2 {
		res := <-results
		require.NoError(t, res.err)
		switch res.status {
		case http.StatusOK:
			wins++
			winnerPriority = res.priority
		case http.StatusPreconditionFailed:
			conflicts++
			assertAPIError(t, res.status, res.body, http.StatusPreconditionFailed, "revision_conflict")
		default:
			t.Fatalf("unexpected status %d for priority %d: %s", res.status, res.priority, res.body)
		}
	}
	require.Equal(t, 1, wins, "exactly one concurrent priority edit must succeed")
	require.Equal(t, 1, conflicts, "the losing concurrent priority edit must return 412 revision_conflict")

	stored, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Priority)
	assert.Equal(t, winnerPriority, *stored.Priority)
	assert.Equal(t, issue.Revision+1, stored.Revision)
}

func TestConcurrentIssuePriorityIfMatchSQLite(t *testing.T) {
	env := testenv.New(t)
	runConcurrentPriorityEditCheck(t, env.URL, env.DB)
}

func TestConcurrentIssuePriorityIfMatchPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := t.Context()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	srv := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	runConcurrentPriorityEditCheck(t, httpSrv.URL, store)
}
