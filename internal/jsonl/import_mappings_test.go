package jsonl_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

func TestExportImport_DeletedIssueOmitsImportMappings(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "kata")
	require.NoError(t, err)
	live, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "live issue", Author: "tester"})
	require.NoError(t, err)
	deleted, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "deleted issue", Author: "tester", Labels: []string{"deleted-label"}})
	require.NoError(t, err)
	comment, _, err := src.CreateComment(ctx, db.CreateCommentParams{IssueID: deleted.ID, Author: "tester", Body: "deleted comment"})
	require.NoError(t, err)
	link, _, err := src.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: deleted.ID,
		ToIssueID:   live.ID,
		Type:        "blocks",
		Author:      "tester",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: deleted.ID,
		FromShortID:  deleted.ShortID, FromUID: deleted.UID,
		ToShortID: live.ShortID, ToUID: live.UID,
		Actor: "tester",
	})
	require.NoError(t, err)
	label := "deleted-label"

	for _, params := range []db.ImportMappingParams{
		{Source: "beads", ExternalID: "live-issue", ObjectType: "issue", ProjectID: p.ID, IssueID: &live.ID},
		{Source: "beads", ExternalID: "deleted-issue", ObjectType: "issue", ProjectID: p.ID, IssueID: &deleted.ID},
		{Source: "beads", ExternalID: "deleted-comment", ObjectType: "comment", ProjectID: p.ID, IssueID: &deleted.ID, CommentID: &comment.ID},
		{Source: "beads", ExternalID: "deleted-label", ObjectType: "label", ProjectID: p.ID, IssueID: &deleted.ID, Label: &label},
		{Source: "beads", ExternalID: "deleted-link", ObjectType: "link", ProjectID: p.ID, IssueID: &deleted.ID, LinkID: &link.ID},
	} {
		_, err := src.UpsertImportMapping(ctx, params)
		require.NoError(t, err)
	}
	_, _, _, err = src.SoftDeleteIssue(ctx, deleted.ID, "tester")
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{IncludeDeleted: false}))
	exported := buf.String()
	assert.Contains(t, exported, `"external_id":"live-issue"`)
	assert.NotContains(t, exported, `"external_id":"deleted-issue"`)
	assert.NotContains(t, exported, `"external_id":"deleted-comment"`)
	assert.NotContains(t, exported, `"external_id":"deleted-label"`)
	assert.NotContains(t, exported, `"external_id":"deleted-link"`)

	dst := openExportTestDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dst))

	got, err := dst.ImportMappingsByProjectSource(ctx, p.ID, "beads")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "live-issue", got[0].ExternalID)
}

func TestExportImport_PreservesImportMappings(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "kata")
	require.NoError(t, err)
	issue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "from beads", Author: "tester"})
	require.NoError(t, err)
	comment, _, err := src.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "comment"})
	require.NoError(t, err)
	srcUpdated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	_, err = src.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source:          "beads",
		ExternalID:      "issue-1",
		ObjectType:      "issue",
		ProjectID:       p.ID,
		IssueID:         &issue.ID,
		SourceUpdatedAt: &srcUpdated,
	})
	require.NoError(t, err)
	_, err = src.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source:     "beads",
		ExternalID: "comment-1",
		ObjectType: "comment",
		ProjectID:  p.ID,
		IssueID:    &issue.ID,
		CommentID:  &comment.ID,
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{IncludeDeleted: true}))
	assert.Contains(t, buf.String(), `"kind":"import_mapping"`)

	dst := openExportTestDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dst))

	got, err := dst.ImportMappingsByProjectSource(ctx, p.ID, "beads")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "issue-1", got[0].ExternalID)
	assert.Equal(t, "comment-1", got[1].ExternalID)
	require.NotNil(t, got[0].SourceUpdatedAt)
	assert.True(t, got[0].SourceUpdatedAt.Equal(srcUpdated), "got %s, want %s", got[0].SourceUpdatedAt, srcUpdated)
}
