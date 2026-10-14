package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type commentGraphWorkStore struct {
	db.Storage
	graphs []db.CommentGraphData
}

func (s *commentGraphWorkStore) ReadCommentGraph(ctx context.Context, query db.CommentGraphQuery) (db.CommentGraphData, error) {
	data, err := s.Storage.ReadCommentGraph(ctx, query)
	s.graphs = append(s.graphs, data)
	return data, err
}

func TestSelectedCommentGraphSkipsUnrelatedBodiesAndTargets(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	selected := createScopedAuthIssue(t, store, project.ID, "Selected", nil)
	other := createScopedAuthIssue(t, store, project.ID, "Other", nil)
	finding, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: other.ID, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: selected.ID, Author: "worker", Body: "Selected reply", ReplyToUID: finding.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	comment.UID = "01AAAAAAAAAAAAAAAAAAABCDEF"
	_, err = store.ExecContext(t.Context(), `UPDATE comments SET uid=? WHERE id=?`, comment.UID, comment.ID)
	require.NoError(t, err)
	backlink, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: other.ID, Author: "reviewer", Body: "Incoming evidence", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	for index := range 32 {
		unrelated, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: other.ID, Author: "worker", Body: strings.Repeat("Unrelated", 1024), ReplyToUID: fmt.Sprintf("01M6%022d", index), ReplyKind: "reply"})
		require.NoError(t, err)
		if index == 0 {
			_, err = store.ExecContext(t.Context(), `UPDATE comments SET uid=? WHERE id=?`, "01BBBBBBBBBBBBBBBBBBABCDEF", unrelated.ID)
			require.NoError(t, err)
		}
	}
	counted := &commentGraphWorkStore{Storage: store}
	out, err := hydrateShowIssueResponse(t.Context(), ServerConfig{DB: counted, InsecureReadonly: true}, selected, false)
	require.NoError(t, err)
	require.Len(t, out.Body.Comments, 1)
	require.Equal(t, "c:aabcdef", out.Body.Comments[0].Handle, "an unrelated visible collision still extends the selected handle")
	require.Equal(t, finding.Body, out.Body.Comments[0].Reply.Body)
	require.Len(t, out.Body.Comments[0].Backlinks, 1)
	require.Equal(t, backlink.Body, out.Body.Comments[0].Backlinks[0].Body)
	require.Len(t, counted.graphs, 1)
	require.Len(t, counted.graphs[0].Comments, 3, "only the selected comment and its direct endpoints need bodies")
	require.Empty(t, counted.graphs[0].Targets, "unrelated pending endpoints need no evidence queries")

	snapshot, err := store.ReadUISnapshot(t.Context(), db.UISnapshotQuery{ProjectUID: project.UID, SelectedIssueUID: selected.UID})
	require.NoError(t, err)
	require.Len(t, snapshot.CommentGraph.Comments, 3)
	require.Empty(t, snapshot.CommentGraph.Targets)
	require.Len(t, snapshot.Comments, 1)
	require.Equal(t, comment.UID, snapshot.Comments[0].UID)
}

func TestCommentReferenceWritesReadOnlyMatchingIdentity(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue := createScopedAuthIssue(t, store, project.ID, "Finding", nil)
	target, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	for index := range 16 {
		_, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Unrelated", ReplyToUID: fmt.Sprintf("01M6%022d", index), ReplyKind: "reply"})
		require.NoError(t, err)
	}
	counted := &commentGraphWorkStore{Storage: store}
	server := NewServer(ServerConfig{DB: counted, StartedAt: time.Now()})
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			counted.graphs = nil
			path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", project.ID, issue.ShortID)
			body := map[string]any{"actor": "finder", "body": "Updated finding"}
			handle := "c:" + strings.ToLower(target.UID[20:])
			if method == http.MethodPost {
				body["reply_to"], body["kind"] = handle, "reply"
			} else {
				path += "/" + handle
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			request := httptest.NewRequest(method, path, bytes.NewReader(raw))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.baseHandler.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), target.UID)
			require.Len(t, counted.graphs, 1)
			require.Len(t, counted.graphs[0].Comments, 1)
			require.Equal(t, target.UID, counted.graphs[0].Comments[0].Comment.UID)
			require.Empty(t, counted.graphs[0].Comments[0].Comment.Body, "reference resolution only needs identity")
			require.Empty(t, counted.graphs[0].Targets)
			require.Empty(t, counted.graphs[0].CommentUIDsByProject)
		})
	}
}

func TestImportedCommentReferenceCase(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
			store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			project, err := store.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			issue := createScopedAuthIssue(t, store, project.ID, "Imported finding", nil)
			target, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "finder", Body: "Original finding"})
			require.NoError(t, err)
			records, err := dbtest.CollectImportRecords(t.Context(), store, db.ExportFilter{ProjectID: &project.ID})
			require.NoError(t, err)
			persistedUID := strings.ToLower(target.UID)
			if mixed {
				persistedUID = strings.ToLower(target.UID[:13]) + target.UID[13:]
			}
			for _, record := range records {
				if comment, ok := record.(*db.CommentExport); ok && comment.UID == target.UID {
					comment.UID = persistedUID
				}
			}
			require.NoError(t, store.ImportReplay(t.Context(), records, db.ImportOptions{}))
			server := NewServer(ServerConfig{DB: store, StartedAt: time.Now()})
			for _, ref := range []string{target.UID, "c:" + strings.ToLower(target.UID[20:])} {
				for _, method := range []string{http.MethodPost, http.MethodPatch} {
					t.Run(method+"/"+ref, func(t *testing.T) {
						path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", project.ID, issue.ShortID)
						body := map[string]any{"actor": "finder", "body": "Updated finding"}
						if method == http.MethodPost {
							body["reply_to"], body["kind"], body["force"] = ref, "reply", true
						} else {
							path += "/" + ref
						}
						raw, err := json.Marshal(body)
						require.NoError(t, err)
						request := httptest.NewRequest(method, path, bytes.NewReader(raw))
						request.Header.Set("Content-Type", "application/json")
						response := httptest.NewRecorder()
						server.baseHandler.ServeHTTP(response, request)
						require.Equal(t, http.StatusOK, response.Code, response.Body.String())
						var out struct {
							Comment db.Comment `json:"comment"`
						}
						require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
						if method == http.MethodPost {
							require.Equal(t, persistedUID, out.Comment.ReplyToUID)
						} else {
							require.Equal(t, persistedUID, out.Comment.UID)
							require.Equal(t, "Updated finding", out.Comment.Body)
						}
					})
				}
			}
		})
	}
}
