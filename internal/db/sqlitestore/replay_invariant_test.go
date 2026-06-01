package sqlitestore_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestReplayInvariant_ProjectProjectionMatchesDirectWrites(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "original",
		Body:      "old body",
		Author:    "agent",
	})
	require.NoError(t, err)
	b, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "peer",
		Author:    "agent",
	})
	require.NoError(t, err)

	newTitle := "current title"
	newBody := "current body"
	owner := "alice"
	_, _, changed, err := d.EditIssue(ctx, db.EditIssueParams{
		IssueID: a.ID,
		Title:   &newTitle,
		Body:    &newBody,
		Owner:   &owner,
		Actor:   "agent",
	})
	require.NoError(t, err)
	require.True(t, changed)

	priority := int64(2)
	_, _, changed, err = d.UpdatePriority(ctx, a.ID, &priority, "agent")
	require.NoError(t, err)
	require.True(t, changed)

	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: a.ID,
		Author:  "agent",
		Body:    "note",
	})
	require.NoError(t, err)

	_, _, err = d.AddLabelAndEvent(ctx, a.ID, db.LabelEventParams{
		EventType: "issue.labeled",
		Label:     "bug",
		Actor:     "agent",
	})
	require.NoError(t, err)

	_, _, err = d.CreateLinkAndEvent(ctx,
		db.CreateLinkParams{
			ProjectID:   p.ID,
			FromIssueID: a.ID,
			ToIssueID:   b.ID,
			Type:        "blocks",
			Author:      "agent",
		},
		db.LinkEventParams{
			EventType:    "issue.linked",
			EventIssueID: a.ID,
			FromShortID:  a.ShortID,
			FromUID:      a.UID,
			ToShortID:    b.ShortID,
			ToUID:        b.UID,
			Actor:        "agent",
		})
	require.NoError(t, err)

	metadataOut, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID:    a.ID,
		IfMatchRev: 1,
		Actor:      "agent",
		Patch:      map[string]json.RawMessage{"area": json.RawMessage(`"api"`)},
	})
	require.NoError(t, err)
	require.True(t, metadataOut.Changed)

	_, _, changed, err = d.CloseIssue(ctx, a.ID, "done", "agent", "", nil)
	require.NoError(t, err)
	require.True(t, changed)
	_, _, changed, err = d.ReopenIssue(ctx, a.ID, "agent")
	require.NoError(t, err)
	require.True(t, changed)
	_, _, changed, err = d.SoftDeleteIssue(ctx, a.ID, "agent")
	require.NoError(t, err)
	require.True(t, changed)
	_, _, changed, err = d.RestoreIssue(ctx, a.ID, "agent")
	require.NoError(t, err)
	require.True(t, changed)

	got := db.FoldEvents(loadFoldEventsForProject(ctx, t, d, p.ID))
	current, err := d.IssueByID(ctx, a.ID)
	require.NoError(t, err)
	folded := got.Issues[a.UID]
	assert.Equal(t, current.UID, folded.UID)
	assert.Equal(t, current.ShortID, folded.ShortID)
	assert.Equal(t, current.Title, folded.Title)
	assert.Equal(t, current.Body, folded.Body)
	assert.Equal(t, current.Author, folded.Author)
	assert.Equal(t, current.Status, folded.Status)
	assert.Equal(t, current.ProjectUID, folded.ProjectUID)
	assert.Equal(t, current.Owner, folded.Owner)
	assert.Equal(t, current.Priority, folded.Priority)
	assert.Equal(t, current.ClosedReason, folded.ClosedReason)
	assert.Equal(t, timeValueForFold(current.ClosedAt), folded.ClosedAt)
	assert.Equal(t, timeValueForFold(current.DeletedAt), folded.DeletedAt)
	assert.Equal(t, current.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"), folded.UpdatedAt)

	foldedComment := got.Comments[comment.UID]
	assert.Equal(t, a.UID, foldedComment.IssueUID)
	assert.Equal(t, comment.Author, foldedComment.Author)
	assert.Equal(t, comment.Body, foldedComment.Body)
	assert.Equal(t, comment.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"), foldedComment.CreatedAt)
	assert.True(t, got.Labels[db.FoldLabelKey{IssueUID: a.UID, Label: "bug"}].Present)
	assert.True(t, got.Links[db.FoldLinkKey{FromUID: a.UID, ToUID: b.UID, Type: "blocks"}].Present)
	assert.JSONEq(t, string(current.Metadata), string(got.IssueMetadata[a.UID]))
}

func loadFoldEventsForProject(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64) []db.FoldEvent {
	t.Helper()
	events, err := d.EventsAfter(ctx, db.EventsAfterParams{
		AfterID:   0,
		ProjectID: projectID,
		Limit:     1000,
	})
	require.NoError(t, err)
	out := make([]db.FoldEvent, 0, len(events))
	for _, ev := range events {
		out = append(out, db.FoldEvent{
			UID:               ev.UID,
			OriginInstanceUID: ev.OriginInstanceUID,
			ProjectUID:        ev.ProjectUID,
			IssueUID:          stringValueForFold(ev.IssueUID),
			RelatedIssueUID:   stringValueForFold(ev.RelatedIssueUID),
			Type:              ev.Type,
			Actor:             ev.Actor,
			HLCPhysicalMS:     ev.HLCPhysicalMS,
			HLCCounter:        ev.HLCCounter,
			CreatedAt:         ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			Payload:           json.RawMessage(ev.Payload),
		})
	}
	return out
}

func stringValueForFold(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func timeValueForFold(t *time.Time) *string {
	if t == nil {
		return nil
	}
	v := t.UTC().Format("2006-01-02T15:04:05.000Z")
	return &v
}
