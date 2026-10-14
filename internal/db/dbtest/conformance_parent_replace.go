package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunParentReplacementMissingExpectedLink checks a concurrent removal between
// handler preflight and the atomic parent replacement returns a conflict.
func RunParentReplacementMissingExpectedLink(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Child task", Author: "example-actor",
	})
	require.NoError(t, err)
	oldParent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Old parent", Author: "example-actor",
	})
	require.NoError(t, err)
	newParent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "New parent", Author: "example-actor",
	})
	require.NoError(t, err)
	oldLink, _, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: child.ID, ToIssueID: oldParent.ID, Type: "parent", Author: "example-actor",
	}, db.LinkEventParams{
		EventType: "issue.linked", EventIssueID: child.ID,
		FromShortID: child.ShortID, FromUID: child.UID,
		ToShortID: oldParent.ShortID, ToUID: oldParent.UID, Actor: "example-actor",
	})
	require.NoError(t, err)
	_, err = store.DeleteLinkAndEvent(ctx, oldLink, db.LinkEventParams{
		EventType: "issue.unlinked", EventIssueID: child.ID,
		FromShortID: child.ShortID, FromUID: child.UID,
		ToShortID: oldParent.ShortID, ToUID: oldParent.UID, Actor: "example-actor",
	})
	require.NoError(t, err)

	replacement, ok := store.(db.ParentLinkReplacementStorage)
	require.True(t, ok, "native stores support atomic parent replacement")
	_, err = replacement.ReplaceParentAndEvents(ctx, db.ReplaceParentAndEventsParams{
		ExpectedParentLinkID: oldLink.ID, ExpectedParentIssueID: oldParent.ID,
		Link: db.CreateLinkParams{
			FromIssueID: child.ID, ToIssueID: newParent.ID, Type: "parent", Author: "example-actor",
		},
		UnlinkEvent: db.LinkEventParams{
			EventType: "issue.unlinked", EventIssueID: child.ID,
			FromShortID: child.ShortID, FromUID: child.UID,
			ToShortID: oldParent.ShortID, ToUID: oldParent.UID, Actor: "example-actor",
		},
		LinkEvent: db.LinkEventParams{
			EventType: "issue.linked", EventIssueID: child.ID,
			FromShortID: child.ShortID, FromUID: child.UID,
			ToShortID: newParent.ShortID, ToUID: newParent.UID, Actor: "example-actor",
		},
	})
	require.ErrorIs(t, err, db.ErrParentMismatch)
	_, err = store.ParentOf(ctx, child.ID)
	require.ErrorIs(t, err, db.ErrNotFound, "a failed replacement must not install the new parent")
}
