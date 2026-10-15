package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

// These are the broadcast product contracts: local direct-child discovery,
// atomic 50-recipient refusal, and write-transaction serialization of limits.
func TestNotifyBroadcastBackendContracts(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store db.Storage
			var err error
			if backend == "postgres" {
				if testing.Short() {
					t.Skip("requires postgres testcontainer")
				}
				dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
				t.Cleanup(cleanup)
				store, err = pgstore.Open(t.Context(), dsn)
			} else {
				store, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "example.db"))
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			d := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
			t.Cleanup(func() { _ = d.Close() })
			server := httptest.NewServer(d.Handler())
			t.Cleanup(server.Close)
			p, err := store.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			create := func(title, owner string, parent *db.Issue) db.Issue {
				in := db.CreateIssueParams{ProjectID: p.ID, Title: title, Author: "coordinator"}
				if parent != nil {
					in.Links = []db.InitialLink{{Type: "parent", ToNumber: parent.ID}}
				}
				issue, _, err := store.CreateIssue(t.Context(), in)
				require.NoError(t, err)
				if owner != "" {
					_, _, _, err = store.UpdateOwner(t.Context(), issue.ID, &owner, "coordinator")
					require.NoError(t, err)
				}
				return issue
			}
			root := create("Root", "root-owner", nil)
			child := create("Direct child", "child-owner", &root)
			closed := create("Closed child", "closed-owner", &root)
			create("Grandchild", "grandchild-owner", &child)
			_, _, _, err = store.CloseIssue(t.Context(), closed.ID, "done", "coordinator", "Completed child work", nil)
			require.NoError(t, err)
			for _, author := range []string{"sender", "reader", "system", "github-sync"} {
				_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: root.ID, Author: author, Body: "Finding"})
				require.NoError(t, err)
			}
			c, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: child.ID, Author: "imported-author", Body: "Imported finding"})
			require.NoError(t, err)
			_, err = store.UpsertImportMapping(t.Context(), db.ImportMappingParams{ProjectID: p.ID, Source: "example-source", ExternalID: "example-comment", ObjectType: "comment", IssueID: &child.ID, CommentID: &c.ID})
			require.NoError(t, err)
			_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: child.ID, Author: "child-reader", Body: "Finding"})
			require.NoError(t, err)
			path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", p.ID, root.ShortID)
			response, body := postJSON(t, server, path, map[string]any{"actor": "sender", "broadcast": true, "message": "Inspect findings"})
			require.Equal(t, 200, response.StatusCode, string(body))
			var result struct {
				Recipients []string `json:"recipients"`
			}
			require.NoError(t, json.Unmarshal(body, &result))
			require.Equal(t, []string{"child-owner", "child-reader", "reader", "root-owner"}, result.Recipients)
			for _, issue := range []db.Issue{child, closed} {
				response, body = postJSON(t, server, fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", p.ID, issue.ShortID), map[string]any{"actor": "sender", "broadcast": true, "message": "Inspect"})
				if issue.ID == closed.ID {
					require.Equal(t, 400, response.StatusCode, string(body))
				} else {
					require.Equal(t, 200, response.StatusCode, string(body))
				}
			}
			capped := create("Recipient cap", "", nil)
			for i := range 51 {
				_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: capped.ID, Author: fmt.Sprintf("worker-%02d", i), Body: "Finding"})
				require.NoError(t, err)
			}
			before, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			response, body = postJSON(t, server, fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", p.ID, capped.ShortID), map[string]any{"actor": "sender", "broadcast": true, "message": "Inspect"})
			require.Equal(t, 400, response.StatusCode, string(body))
			require.Contains(t, string(body), "broadcast_recipient_limit")
			after, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)

			concurrent := create("Concurrent broadcast", "reader", nil)
			start := make(chan struct{})
			var ready sync.WaitGroup
			ready.Add(2)
			type outcome struct {
				status int
				body   []byte
				err    error
			}
			outcomes := make(chan outcome, 2)
			for range 2 {
				go func() {
					ready.Done()
					<-start
					raw := []byte(`{"actor":"sender","broadcast":true,"message":"Inspect concurrently"}`)
					req, e := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/notifications", server.URL, p.ID, concurrent.ShortID), bytes.NewReader(raw))
					if e != nil {
						outcomes <- outcome{err: e}
						return
					}
					req.Header.Set("Content-Type", "application/json")
					resp, e := server.Client().Do(req)
					if e != nil {
						outcomes <- outcome{err: e}
						return
					}
					body, e := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					outcomes <- outcome{status: resp.StatusCode, body: body, err: e}
				}()
			}
			ready.Wait()
			close(start)
			statuses := map[int]int{}
			for range 2 {
				result := <-outcomes
				require.NoError(t, result.err)
				require.Contains(t, []int{200, 429}, result.status, string(result.body))
				statuses[result.status]++
			}
			require.Equal(t, map[int]int{200: 1, 429: 1}, statuses)
		})
	}
}
