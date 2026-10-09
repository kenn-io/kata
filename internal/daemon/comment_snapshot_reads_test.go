package daemon_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type snapshotCommentLookupStore struct {
	db.Storage
	db.UIStore
	lookups atomic.Int64
}

func (s *snapshotCommentLookupStore) CommentIssueIDsByUIDs(ctx context.Context, uids []string) (map[string]int64, error) {
	s.lookups.Add(1)
	return s.Storage.CommentIssueIDsByUIDs(ctx, uids)
}

func TestScopedSnapshotAvoidsLegacyCommentProjection(t *testing.T) {
	counted := &snapshotCommentLookupStore{}
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity(),
		func(cfg *daemon.ServerConfig) {
			counted.Storage = cfg.DB
			counted.UIStore = cfg.DB.(db.UIStore)
			cfg.DB, cfg.UIStore = counted, counted
		})
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Response", &root)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside", nil)
	target, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: outside.ID, Author: "finder", Body: "Hidden finding"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: child.ID, Author: "worker", Body: "Visible response", ReplyToUID: target.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope:     &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	counted.lookups.Store(0)
	resp, body := envDoRaw(t, env, http.MethodGet, "/api/v1/ui/snapshot?view=all-open&include_graph=true&selected_issue_uid="+child.UID, nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, string(body), "Visible response")
	require.NotContains(t, string(body), target.UID)
	require.NotContains(t, string(body), "Hidden finding")
	require.Zero(t, counted.lookups.Load(), "the captured graph already projects replies; unused legacy comment rows must not trigger endpoint lookups")
}
