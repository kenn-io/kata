package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// TestLiveIssueEnforcesProjectFilter pins liveIssue's `i.project_id = ?`
// clause directly. The external-package TestSearchVectorIsolatesProjects
// passes even with that clause removed, because loadVectors already scopes
// candidate retrieval by project — the foreign id never reaches liveIssue
// through SearchVector. This test calls liveIssue with a deliberately
// cross-project id so the project filter is the only thing standing between
// the query and a foreign row.
func TestLiveIssueEnforcesProjectFilter(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KATA_HOME", t.TempDir())
	d, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	projA, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	projB, err := d.CreateProject(ctx, "hub-project")
	require.NoError(t, err)

	_, _, err = d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projA.ID, Title: "a", Body: "b", Author: "tester",
	})
	require.NoError(t, err)
	issB, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projB.ID, Title: "b", Body: "b", Author: "tester",
	})
	require.NoError(t, err)

	// Resolving issue B's id against project A must return (nil, nil): the
	// project filter rejects the foreign id even though the row exists and is
	// live. Without `i.project_id = ?`, this would return B's row.
	got, err := d.liveIssue(ctx, projA.ID, issB.ID, false)
	require.NoError(t, err)
	require.Nilf(t, got, "liveIssue must not resolve an id from another project")

	// Sanity: B's id does resolve under its own project, so the nil above is
	// the project filter at work, not a missing row.
	got, err = d.liveIssue(ctx, projB.ID, issB.ID, false)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, issB.ID, got.ID)
}
