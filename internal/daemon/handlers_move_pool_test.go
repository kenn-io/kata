package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestMoveAuthorizesProjectsWithOneConnection(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		pg, ok := store.(*pgstore.Store)
		if !ok {
			t.Skip("connection-pool exhaustion applies to PostgreSQL")
		}
		for _, single := range []bool{false, true} {
			t.Run(fmt.Sprintf("single=%t", single), func(t *testing.T) {
				f := newProjectAccessFixture(t, store, fmt.Sprint(single))
				target, err := store.CreateProject(t.Context(), "target-"+fmt.Sprint(single))
				require.NoError(t, err)
				_, err = store.CreateLink(t.Context(), db.CreateLinkParams{
					FromIssueID: f.issue.ID, ToIssueID: f.visible.ID, Type: "parent", Author: "member",
				})
				require.NoError(t, err)
				pg.SetMaxOpenConns(1)
				for _, dryRun := range []bool{true, false} {
					body, err := json.Marshal(map[string]any{"to_project_uid": target.UID, "single": single, "dry_run": dryRun})
					require.NoError(t, err)
					bounded, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					req, err := http.NewRequestWithContext(bounded, http.MethodPost,
						fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", f.server.URL, f.public.ID, f.visible.ShortID), bytes.NewReader(body))
					require.NoError(t, err)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer member-"+f.tokenSuffix+"-test-token")
					req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, f.visible.Revision))
					resp, err := f.server.Client().Do(req)
					require.NoError(t, err, "move authorization must use the connection held by its transaction")
					raw, readErr := io.ReadAll(resp.Body)
					require.NoError(t, resp.Body.Close())
					require.NoError(t, readErr)
					require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
					for _, before := range []db.Issue{f.visible, f.issue} {
						after, err := store.IssueByID(t.Context(), before.ID)
						require.NoError(t, err)
						wantProject := target.ID
						if dryRun || single && before.ID == f.issue.ID {
							wantProject = before.ProjectID
						}
						require.Equal(t, wantProject, after.ProjectID)
					}
				}
			})
		}
	})
}
