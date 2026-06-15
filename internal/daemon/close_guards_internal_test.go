package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

// TestCheckParentCloseCompleteness_RendersForeignChildQualified: the
// parent-close refusal lists cross-project children, and a child outside the
// parent's project must render as "project#short_id" — a bare short_id is
// ambiguous across projects and is not actionable from the parent project.
// A same-project child stays bare.
func TestCheckParentCloseCompleteness_RendersForeignChildQualified(t *testing.T) {
	ctx := context.Background()
	store, alpha, beta := auditRenderStore(t)

	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "parent", Author: "tester",
	})
	require.NoError(t, err)
	localChild, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "local child", Author: "tester",
	})
	require.NoError(t, err)
	foreignChild, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: beta.ID, Title: "foreign child", Author: "tester",
	})
	require.NoError(t, err)
	for _, child := range []db.Issue{localChild, foreignChild} {
		_, err = store.CreateLink(ctx, db.CreateLinkParams{
			FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "tester",
		})
		require.NoError(t, err)
	}

	err = CheckParentCloseCompleteness(ctx, store, parent.ID, parent.ShortID, alpha.ID)
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, "beta#"+foreignChild.ShortID,
		"foreign child must render qualified as project#short_id")
	require.Contains(t, msg, localChild.ShortID, "local child must be listed")
	require.NotContains(t, msg, "alpha#"+localChild.ShortID,
		"same-project child must render bare, not qualified")
}
