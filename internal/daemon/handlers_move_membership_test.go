package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type moveMembershipChangeStore struct {
	db.Storage
	beforeCommit func()
}

func (s *moveMembershipChangeStore) MoveIssueProject(ctx context.Context, input db.MoveIssueProjectIn) (db.MoveIssueProjectOut, error) {
	if !input.DryRun && s.beforeCommit != nil {
		change := s.beforeCommit
		s.beforeCommit = nil
		change()
	}
	return s.Storage.MoveIssueProject(ctx, input)
}

func TestMoveIssueRejectsClaimedDescendantAttachedAfterValidation(t *testing.T) {
	var wrapped *moveMembershipChangeStore
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		wrapped = &moveMembershipChangeStore{Storage: cfg.DB}
		cfg.DB = wrapped
	})
	source, root := createClaimHubIssueNamed(t, env, "source-project")
	target, _ := createClaimHubIssueNamed(t, env, "target-project")
	child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "independently claimed child", Author: "worker"})
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, source, child, "other")
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	attached := false
	wrapped.beforeCommit = func() {
		_, err := env.DB.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "worker"})
		require.NoError(t, err)
		current, err := env.DB.IssueByID(t.Context(), root.ID)
		require.NoError(t, err)
		require.Equal(t, root.Revision, current.Revision, "attaching a child does not change the root revision")
		attached = true
	}
	response, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(source.ID, root.ShortID, "actions/move"),
		map[string]any{"actor": "worker", "to_project_uid": target.UID}, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, root.Revision)})
	require.True(t, attached)
	assertAPIError(t, response.StatusCode, raw, http.StatusConflict, "issue_move_tree_changed")
	for _, original := range []db.Issue{root, child} {
		stored, err := env.DB.IssueByID(t.Context(), original.ID)
		require.NoError(t, err)
		require.Equal(t, original.ProjectID, stored.ProjectID)
		require.Equal(t, original.Revision, stored.Revision)
	}
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused move emits no events")
}
