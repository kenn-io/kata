package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func checkTruncatedIncomingScope(t *testing.T, first, second uint32) {
	t.Helper()
	if first == second {
		return
	}
	rootID, omittedID, retainedID := int64(first)+1, int64(second)+1, int64(first)+int64(second)+3
	finding := db.Comment{UID: "01AAAAAAAAAAAAAAAAAA000000", IssueID: rootID, Body: "Finding"}
	rows := []db.CommentGraphRecord{{Comment: finding, IssueUID: "finding", IssueShortID: "aaaa"}}
	for n := 1; n <= 51; n++ {
		issueID, issueUID := retainedID, "responses"
		if n == 1 {
			issueID, issueUID = omittedID, "omitted"
		}
		rows = append(rows, db.CommentGraphRecord{
			Comment: db.Comment{UID: fmt.Sprintf("01AAAAAAAAAAAAAAAAAA%06d", n), IssueID: issueID,
				Body: "Evidence", CreatedAt: time.Unix(int64(n), 0), ReplyToUID: finding.UID, ReplyKind: "reply"},
			IssueUID: issueUID, IssueShortID: "bbbb",
		})
	}
	ctx := db.WithIssueScopeTargets(t.Context())
	out, err := snapshotResponse(ctx, db.UISnapshotData{
		SelectedIssue: &db.UIIssue{UID: "finding", ID: rootID}, CommentGraph: db.CommentGraphData{Comments: rows},
	}, normalizedUISnapshotIntent{SelectedIssueUID: "finding"}, uiPolicy{}, "validator")
	require.NoError(t, err)
	require.Len(t, out.Body.Selected.Comments, 1)
	require.True(t, out.Body.Selected.Comments[0].BacklinksTruncated)
	require.ElementsMatch(t, []int64{rootID, omittedID, retainedID}, db.IssueScopeTargets(ctx),
		"partial-count metadata must revalidate contributors omitted from the evidence window")
}

func TestTruncatedIncomingEvidenceRevalidatesOmittedContributor(t *testing.T) {
	checkTruncatedIncomingScope(t, 0, 1)
}

func FuzzTruncatedIncomingEvidenceScope(f *testing.F) {
	f.Add(uint32(0), uint32(1))
	f.Fuzz(func(t *testing.T, first, second uint32) {
		checkTruncatedIncomingScope(t, first, second)
	})
}

// Every issue exposed by a comment graph must participate in the buffered
// response authorization check, including endpoints represented only by links.
func FuzzSnapshotCommentEndpointsRevalidated(f *testing.F) {
	f.Add(uint32(1), uint32(2), false)
	f.Add(uint32(7), uint32(19), true)
	f.Fuzz(func(t *testing.T, first, second uint32, showReply bool) {
		if first == second {
			return
		}
		finding := db.Comment{UID: "01AAAAAAAAAAAAAAAAAAAAAAAA", IssueID: int64(first) + 1, Body: "Finding"}
		reply := db.Comment{UID: "01BBBBBBBBBBBBBBBBBBBBBBBB", IssueID: int64(second) + 1, Body: "Answer", ReplyToUID: finding.UID, ReplyKind: "reply"}
		selected := "finding"
		if showReply {
			selected = "reply"
		}
		ctx := db.WithIssueScopeTargets(t.Context())
		data := db.UISnapshotData{SelectedIssue: &db.UIIssue{UID: selected}, CommentGraph: db.CommentGraphData{Comments: []db.CommentGraphRecord{
			{Comment: finding, IssueUID: "finding", IssueShortID: "aaaa"},
			{Comment: reply, IssueUID: "reply", IssueShortID: "bbbb"},
		}}}
		out, err := snapshotResponse(ctx, data, normalizedUISnapshotIntent{SelectedIssueUID: selected}, uiPolicy{}, "validator")
		require.NoError(t, err)
		require.Len(t, out.Body.Selected.Comments, 1)
		require.ElementsMatch(t, []int64{finding.IssueID, reply.IssueID}, db.IssueScopeTargets(ctx))
	})
}

func TestCommentGraphResponseRejectsReparentedBacklink(t *testing.T) {
	for _, read := range []string{"show", "thread", "inbound", "snapshot"} {
		t.Run(read, func(t *testing.T) {
			store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			project, err := store.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			root := createScopedAuthIssue(t, store, project.ID, "Finding", nil)
			container := createScopedAuthIssue(t, store, project.ID, "Responses", &root)
			child := createScopedAuthIssue(t, store, project.ID, "Response", &container)
			finding, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: root.ID, Author: "finder", Body: "Finding"})
			require.NoError(t, err)
			reply, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: child.ID, Author: "worker", Body: "Answer", ReplyToUID: finding.UID, ReplyKind: "reply"})
			require.NoError(t, err)
			ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
			handler := withScopedPrincipalRevalidation(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, authorizeIssueScopedIssue(r.Context(), store, root))
				var comments []commentref.Record
				if read == "snapshot" {
					graph, err := store.ReadCommentGraph(r.Context(), db.CommentGraphQuery{ProjectID: project.ID, IssueScope: issueScopeFromContext(r.Context())})
					require.NoError(t, err)
					data := db.UISnapshotData{SelectedIssue: &db.UIIssue{Issue: root}, CommentGraph: graph}
					out, err := snapshotResponse(r.Context(), data, normalizedUISnapshotIntent{SelectedIssueUID: root.UID}, uiPolicy{}, "validator")
					require.NoError(t, err)
					comments = out.Body.Selected.Comments
				} else {
					opts := commentref.Options{}
					switch read {
					case "thread":
						opts.Thread = finding.UID
					case "inbound":
						opts.Inbound = "finder"
					}
					out, err := hydrateShowIssueResponse(r.Context(), ServerConfig{DB: store, InsecureReadonly: true}, root, false, opts)
					require.NoError(t, err)
					comments = out.Body.Comments
				}
				body, err := json.Marshal(comments)
				require.NoError(t, err)
				require.Contains(t, string(body), reply.UID)
				parent, err := store.ParentOf(t.Context(), child.ID)
				require.NoError(t, err)
				require.NoError(t, store.DeleteLinkByID(t.Context(), parent.ID))
				_, err = w.Write(body)
				require.NoError(t, err)
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
			require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
			require.NotContains(t, response.Body.String(), reply.UID)
		})
	}
}

func TestDuplicateReplyResponseRejectsReparentedExistingReply(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	finding, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: root.ID, Author: "finder", Body: "Finding",
	})
	require.NoError(t, err)
	existingIssue := createScopedAuthIssue(t, store, project.ID, "Existing response", &root)
	requestIssue := createScopedAuthIssue(t, store, project.ID, "Retry response", &root)
	existingReply, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: existingIssue.ID, Author: "worker-a", Body: "Existing evidence",
		ReplyToUID: finding.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	parent, err := store.ParentOf(t.Context(), existingIssue.ID)
	require.NoError(t, err)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	storeWithReparent := &reparentAfterCommentGraphReadStore{
		Storage: store, parentLinkID: parent.ID, reparentAfter: 2,
	}
	server := NewServer(ServerConfig{DB: storeWithReparent, StartedAt: time.Now()})
	handler := withScopedPrincipalRevalidation(storeWithReparent, server.baseHandler)
	body, err := json.Marshal(map[string]any{
		"actor": "worker-a", "body": "Retry evidence",
		"reply_to": finding.UID, "kind": "reply",
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", project.ID, requestIssue.ShortID),
		bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(ctx))

	require.Equal(t, 404, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), existingReply.UID,
		"a duplicate reply outside the current subtree must not expose its identity")
	require.Equal(t, 2, storeWithReparent.graphReads,
		"the target-resolution and duplicate-response graph reads must both occur")
}

type reparentAfterCommentGraphReadStore struct {
	db.Storage
	parentLinkID  int64
	reparentAfter int
	graphReads    int
}

func (s *reparentAfterCommentGraphReadStore) ReadCommentGraph(
	ctx context.Context, query db.CommentGraphQuery,
) (db.CommentGraphData, error) {
	data, err := s.Storage.ReadCommentGraph(ctx, query)
	if err != nil {
		return db.CommentGraphData{}, err
	}
	s.graphReads++
	if s.graphReads == s.reparentAfter {
		if err := s.DeleteLinkByID(ctx, s.parentLinkID); err != nil {
			return db.CommentGraphData{}, err
		}
	}
	return data, nil
}
