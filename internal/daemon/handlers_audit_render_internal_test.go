package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// auditRenderStore opens a fresh store with two projects (alpha is audited,
// beta is the foreign project) and returns the store ready for cross-project
// close-audit assertions.
func auditRenderStore(t *testing.T) (*sqlitestore.Store, db.Project, db.Project) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	alpha, err := store.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := store.CreateProject(ctx, "beta")
	require.NoError(t, err)
	return store, alpha, beta
}

// TestDoAuditCloses_ForeignParentRendersQualified: when an audited project's
// closed child has its parent in another (active) project, the audit row must
// render the parent as "<project>#<short_id>", not the bare short_id. A bare
// suffix is ambiguous across projects and does not round-trip as a --parent
// filter against the audited project.
func TestDoAuditCloses_ForeignParentRendersQualified(t *testing.T) {
	ctx := context.Background()
	store, alpha, beta := auditRenderStore(t)

	parentBeta, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: beta.ID, Title: "foreign parent", Author: "tester",
	})
	require.NoError(t, err)
	childAlpha, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "child in alpha", Author: "tester",
	})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: childAlpha.ID, ToIssueID: parentBeta.ID,
		Type: "parent", Author: "tester",
	})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(ctx, childAlpha.ID, "done", "tester",
		"Closed the cross-project child after verifying the fix.", nil)
	require.NoError(t, err)

	resp, err := doAuditCloses(ctx, ServerConfig{DB: store},
		&api.AuditClosesRequest{ProjectID: alpha.ID})
	require.NoError(t, err)
	require.Len(t, resp.Body.Rows, 1)
	assert.Equal(t, "beta#"+parentBeta.ShortID, resp.Body.Rows[0].Parent,
		"foreign parent must render qualified as project#short_id")
}

// TestDoAuditCloses_SameProjectParentRendersBare: a same-project parent must
// still render as the bare short_id, with no project qualifier.
func TestDoAuditCloses_SameProjectParentRendersBare(t *testing.T) {
	ctx := context.Background()
	store, alpha, _ := auditRenderStore(t)

	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "local parent", Author: "tester",
	})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "local child", Author: "tester",
	})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: parent.ID,
		Type: "parent", Author: "tester",
	})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(ctx, child.ID, "done", "tester",
		"Closed the same-project child after verifying the fix.", nil)
	require.NoError(t, err)

	resp, err := doAuditCloses(ctx, ServerConfig{DB: store},
		&api.AuditClosesRequest{ProjectID: alpha.ID})
	require.NoError(t, err)
	require.Len(t, resp.Body.Rows, 1)
	assert.Equal(t, parent.ShortID, resp.Body.Rows[0].Parent,
		"same-project parent must render as a bare short_id")
}
