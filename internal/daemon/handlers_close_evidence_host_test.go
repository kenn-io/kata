package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type closeEvidenceHostAccess struct {
	deniedProjectID int64
}

func (a closeEvidenceHostAccess) Authorize(_ context.Context, request daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	if a.deniedProjectID != 0 && (request.Operation.AllProjects || slices.Contains(request.Operation.ProjectIDs, a.deniedProjectID)) {
		return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
	}
	return daemon.HostAccessDecision{
		TransactionFence: func(context.Context, db.Transaction) error { return nil },
	}, nil
}

func TestCloseEvidenceRequiresCompleteHostAccess(t *testing.T) {
	for _, reason := range []string{"duplicate", "superseded"} {
		for _, allowed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/allowed=%t", reason, allowed), func(t *testing.T) {
				dbh := openTestDB(t)
				source, err := dbh.db.CreateProject(t.Context(), "source-project")
				require.NoError(t, err)
				target, err := dbh.db.CreateProject(t.Context(), "target-project")
				require.NoError(t, err)
				closing, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: source.ID, Title: "Duplicate request", Author: "agent",
				})
				require.NoError(t, err)
				peer, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: source.ID, Title: "Evidence target", Author: "agent",
				})
				require.NoError(t, err)
				_, err = dbh.db.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
					IssueID: peer.ID, FromProjectID: source.ID, ToProjectID: target.ID,
					IfMatchRev: peer.Revision, Actor: "agent",
				})
				require.NoError(t, err)
				access := closeEvidenceHostAccess{}
				if !allowed {
					access.deniedProjectID = target.ID
				}
				server := daemon.NewServer(daemon.ServerConfig{DB: dbh.db, StartedAt: dbh.now, HostAccess: access})
				t.Cleanup(func() { _ = server.Close() })
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx := daemon.WithPrincipal(r.Context(), daemon.Principal{
						Kind: daemon.PrincipalHost, Subject: "host-user", Actor: "agent",
					})
					server.Handler().ServeHTTP(w, r.WithContext(ctx))
				}))
				t.Cleanup(ts.Close)
				typ := "duplicate-of"
				if reason == "superseded" {
					typ = "superseded-by"
				}
				response := postWithHeader(t, ts, issueURLRef(source.ID, closing.ShortID, "actions/close"), nil, map[string]any{
					"actor": "agent", "reason": reason,
					"message":  "This request is covered by the existing tracked replacement issue.",
					"evidence": []map[string]any{{"type": typ, "issue_ref": peer.ShortID}},
				})
				current, err := dbh.db.IssueByID(t.Context(), closing.ID)
				require.NoError(t, err)
				events, err := dbh.db.EventsAfter(t.Context(), db.EventsAfterParams{
					ProjectID: source.ID, IssueUID: closing.UID, Types: []string{"issue.closed"}, Limit: 10,
				})
				require.NoError(t, err)
				if allowed {
					assert.Equal(t, http.StatusOK, response.status, string(response.body))
					assert.Equal(t, "closed", current.Status)
					assert.Len(t, events, 1)
				} else {
					assertAPIError(t, response.status, response.body, http.StatusNotFound, "not_found")
					assert.NotContains(t, string(response.body), peer.UID)
					assert.NotContains(t, string(response.body), target.UID)
					assert.Equal(t, "open", current.Status)
					assert.Empty(t, events, "denied evidence must not commit a close event")
				}
			})
		}
	}
}
