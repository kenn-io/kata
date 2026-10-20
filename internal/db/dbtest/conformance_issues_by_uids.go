package dbtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// checkIssuesByUIDs pins the batch lookup to IssueByUID's per-UID contract:
// the same rows, deleted-row filtering and project boundary, with unknown or
// hidden UIDs omitted rather than reported as errors.
func checkIssuesByUIDs(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	visible, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	hidden, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	first, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: visible.ID, Title: "First", Body: "first body", Author: "worker"})
	require.NoError(t, err)
	deleted, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: visible.ID, Title: "Deleted", Author: "worker"})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, deleted.ID, "worker")
	require.NoError(t, err)
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hidden.ID, Title: "Other", Author: "worker"})
	require.NoError(t, err)
	const unknown = "01ZZZZZZZZZZZZZZZZZZZZZZZZ"
	uids := []string{first.UID, deleted.UID, other.UID, unknown}

	expect := func(ctx context.Context, include db.IncludeDeleted, want ...string) map[string]db.Issue {
		expected := make(map[string]db.Issue, len(want))
		for _, uid := range want {
			issue, err := store.IssueByUID(ctx, uid, include)
			require.NoError(t, err)
			expected[uid] = issue
		}
		return expected
	}

	got, err := store.IssuesByUIDs(ctx, uids, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, expect(ctx, db.IncludeDeletedNo, first.UID, other.UID), got)

	got, err = store.IssuesByUIDs(ctx, uids, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, expect(ctx, db.IncludeDeletedYes, first.UID, deleted.UID, other.UID), got)

	scoped := db.WithAuthorizedProjects(ctx, []string{visible.UID})
	got, err = store.IssuesByUIDs(scoped, uids, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, expect(scoped, db.IncludeDeletedYes, first.UID, deleted.UID), got, "the project boundary hides other projects' issues")

	got, err = store.IssuesByUIDs(ctx, nil, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Empty(t, got)
	return nil
}
