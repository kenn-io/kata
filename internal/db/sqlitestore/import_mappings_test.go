package sqlitestore_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestImportMapping_UpsertAndLookupIssue(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issue := makeIssue(t, ctx, d, p.ID, "imported", "tester")
	srcUpdated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	m, err := d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source:          "beads",
		ExternalID:      "beads-123",
		ObjectType:      "issue",
		ProjectID:       p.ID,
		IssueID:         &issue.ID,
		SourceUpdatedAt: &srcUpdated,
	})
	require.NoError(t, err)
	assert.Equal(t, "beads", m.Source)
	assert.Equal(t, "beads-123", m.ExternalID)
	require.NotNil(t, m.IssueID)
	assert.Equal(t, issue.ID, *m.IssueID)

	got, err := d.ImportMappingBySource(ctx, p.ID, "beads", "issue", "beads-123")
	require.NoError(t, err)
	assert.Equal(t, m.ID, got.ID)
	require.NotNil(t, got.SourceUpdatedAt)
	assert.True(t, got.SourceUpdatedAt.Equal(srcUpdated))
}

func TestImportMapping_ListByProjectSource(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issue := makeIssue(t, ctx, d, p.ID, "imported", "tester")
	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "hi"})
	require.NoError(t, err)

	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "beads-123", ObjectType: "issue", ProjectID: p.ID, IssueID: &issue.ID,
	})
	require.NoError(t, err)
	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "comment-1", ObjectType: "comment", ProjectID: p.ID, IssueID: &issue.ID, CommentID: &comment.ID,
	})
	require.NoError(t, err)

	got, err := d.ImportMappingsByProjectSource(ctx, p.ID, "beads")
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestImportMapping_PurgeIssueRemovesMappedDependents(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	target := makeIssue(t, ctx, d, p.ID, "mapped", "tester")
	keeper := makeIssue(t, ctx, d, p.ID, "keeper", "tester")
	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "tester", Body: "hi"})
	require.NoError(t, err)
	link := makeLink(ctx, t, d, p.ID, keeper.ID, target.ID, "blocks")
	_, _, err = d.AddLabelAndEvent(ctx, target.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "bug", Actor: "tester"})
	require.NoError(t, err)
	label := "bug"

	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "issue-1", ObjectType: "issue", ProjectID: p.ID, IssueID: &target.ID,
	})
	require.NoError(t, err)
	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "comment-1", ObjectType: "comment", ProjectID: p.ID, IssueID: &target.ID, CommentID: &comment.ID,
	})
	require.NoError(t, err)
	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "link-1", ObjectType: "link", ProjectID: p.ID, IssueID: &target.ID, LinkID: &link.ID,
	})
	require.NoError(t, err)
	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "label-1", ObjectType: "label", ProjectID: p.ID, IssueID: &target.ID, Label: &label,
	})
	require.NoError(t, err)

	_, err = d.PurgeIssue(ctx, target.ID, "tester", nil)
	require.NoError(t, err)

	got, err := d.ImportMappingsByProjectSource(ctx, p.ID, "beads")
	require.NoError(t, err)
	assert.Empty(t, got)
}
