package daemon_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

func TestCreate_LookalikeCommentNoise(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var store db.Storage
			if backend == "sqlite" {
				store = openTestDB(t).db
			} else {
				if testing.Short() {
					t.Skip("requires postgres testcontainer")
				}
				dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
				t.Cleanup(cleanup)
				pg, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, pg.Close()) })
				store = pg
			}
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			const title, body = "login", "crash"
			original, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Body: body, Author: "tester"})
			require.NoError(t, err)
			for i := range 22 {
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("unrelated task %d", i), Author: "tester"})
				require.NoError(t, err)
				_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Body: strings.Repeat("login ", 100), Author: "tester"})
				require.NoError(t, err)
				for range 100 {
					_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Body: "routine progress update", Author: "tester"})
					require.NoError(t, err)
				}
			}
			ts := startTestServer(t, daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
			response := postWithHeader(t, ts, issuesURL(project.ID), nil, map[string]any{"actor": "tester", "title": title, "body": body})
			require.Equal(t, http.StatusConflict, response.status, string(response.body))
			assertAPIError(t, response.status, response.body, http.StatusConflict, "duplicate_candidates")
			require.Contains(t, string(response.body), original.ShortID)
		})
	}
}
