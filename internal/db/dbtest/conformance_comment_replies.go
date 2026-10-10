package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentReplies(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	const target = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		c, e, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Evidence about the finding", ReplyToUID: target, ReplyKind: kind})
		require.NoError(t, err)
		require.Equal(t, target, c.ReplyToUID)
		require.Equal(t, kind, c.ReplyKind)
		require.Nil(t, c.EditedAt)
		var payload struct {
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
		}
		require.NoError(t, json.Unmarshal([]byte(e.Payload), &payload))
		require.Equal(t, target, payload.ReplyToUID)
		require.Equal(t, kind, payload.ReplyKind)
		edited, event, changed, err := store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: c.UID, Actor: "worker", Body: "Updated evidence"})
		require.NoError(t, err)
		require.True(t, changed)
		require.NotNil(t, event)
		var edit struct {
			EditedAt time.Time `json:"edited_at"`
		}
		require.NoError(t, json.Unmarshal([]byte(event.Payload), &edit))
		require.NotNil(t, edited.EditedAt)
		require.True(t, edited.EditedAt.Equal(edit.EditedAt))
		require.Equal(t, target, edited.ReplyToUID)
		require.Equal(t, kind, edited.ReplyKind)
		same, event, changed, err := store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: c.UID, Actor: "worker", Body: edited.Body})
		require.NoError(t, err)
		require.False(t, changed)
		require.Nil(t, event)
		require.Equal(t, edited.EditedAt, same.EditedAt)
	}
	for _, params := range []db.CreateCommentParams{{ReplyToUID: target}, {ReplyKind: "reply"}, {ReplyToUID: "short", ReplyKind: "reply"}, {ReplyToUID: target, ReplyKind: "answer"}} {
		params.IssueID = issue.ID
		params.Author = "worker"
		params.Body = "Invalid link"
		_, _, err := store.CreateComment(ctx, params)
		require.Error(t, err)
	}
	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 4)
	for _, c := range comments {
		require.Equal(t, target, c.ReplyToUID)
		require.NotNil(t, c.EditedAt)
	}
	return nil
}

func checkCommentReplyEnvelope(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Reply issue", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Target issue", Author: "worker"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "worker", Body: "Finding"})
	require.NoError(t, err)
	_, event, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Answer", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	require.Equal(t, &target.ID, event.RelatedIssueID)
	require.Equal(t, &target.UID, event.RelatedIssueUID)
	_, same, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "worker", Body: "Same issue answer", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	require.Nil(t, same.RelatedIssueID)
	return nil
}

func checkCommentReplyFederation(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: p.UID, Enabled: true})
	require.NoError(t, err)
	const issueUID = "01CCCCCCCCCCCCCCCCCCCCCCCC"
	const commentUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	const targetUID = "01BBBBBBBBBBBBBBBBBBBBBBBB"
	const editedAt = "2026-10-07T12:00:00.123456789Z"
	issueUIDValue := issueUID
	snapshot := newRemoteEvent(t, p, &issueUIDValue, "issue.snapshot", "worker", "01DDDDDDDDDDDDDDDDDDDDDDDD", 1, jsontext.Value(`{"uid":"`+issueUID+`","title":"Task","author":"worker","status":"open","created_at":"2026-10-07T11:00:00Z","comments":[{"comment_uid":"`+commentUID+`","author":"worker","body":"Reply","created_at":"2026-10-07T11:00:00Z","reply_to_uid":"`+targetUID+`","reply_kind":"confirm","edited_at":"`+editedAt+`"}]}`))
	_, err = store.InsertRemoteEvent(ctx, p.ID, snapshot)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assertReply := func() {
		comments, err := store.CommentsByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.NotEmpty(t, comments)
		require.Equal(t, targetUID, comments[0].ReplyToUID)
		require.Equal(t, "confirm", comments[0].ReplyKind)
		require.NotNil(t, comments[0].EditedAt)
		require.Equal(t, editedAt, comments[0].EditedAt.UTC().Format(time.RFC3339Nano))
	}
	assertReply()
	target := newRemoteEvent(t, p, &issue.UID, "issue.commented", "worker", snapshot.OriginInstanceUID, 2, jsontext.Value(`{"comment_uid":"`+targetUID+`","author":"worker","body":"Finding","created_at":"2026-10-07T11:01:00Z"}`))
	_, err = store.InsertRemoteEvent(ctx, p.ID, target)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
	assertReply()
	for range 2 {
		require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
		assertReply()
	}
	edit := newRemoteEvent(t, p, &issue.UID, "issue.comment_edited", "worker", snapshot.OriginInstanceUID, 3, jsontext.Value(`{"comment_uid":"`+commentUID+`","body":"Updated reply","edited_at":"2026-10-07T13:00:00.987654321Z"}`))
	_, err = store.InsertRemoteEvent(ctx, p.ID, edit)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, "Updated reply", comments[0].Body)
	require.Equal(t, "2026-10-07T13:00:00.987654321Z", comments[0].EditedAt.UTC().Format(time.RFC3339Nano))
	require.Equal(t, targetUID, comments[0].ReplyToUID)
	return nil
}

func checkCommentReplySnapshot(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	c, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Reply", ReplyToUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", ReplyKind: "supersede"})
	require.NoError(t, err)
	c, _, _, err = store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: c.UID, Actor: "worker", Body: "Revised reply"})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, p.ID, "worker")
	require.NoError(t, err)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 100})
	require.NoError(t, err)
	found := false
	for _, event := range events {
		if event.Type != "issue.snapshot" {
			continue
		}
		var snapshot struct {
			Comments []struct {
				ReplyToUID string `json:"reply_to_uid"`
				ReplyKind  string `json:"reply_kind"`
				EditedAt   string `json:"edited_at"`
			} `json:"comments"`
		}
		require.NoError(t, json.Unmarshal([]byte(event.Payload), &snapshot))
		require.Len(t, snapshot.Comments, 1)
		require.Equal(t, c.ReplyToUID, snapshot.Comments[0].ReplyToUID)
		require.Equal(t, c.ReplyKind, snapshot.Comments[0].ReplyKind)
		parsed, err := time.Parse(time.RFC3339Nano, snapshot.Comments[0].EditedAt)
		require.NoError(t, err)
		require.True(t, c.EditedAt.Equal(parsed))
		found = true
	}
	require.True(t, found)
	return nil
}

// Replies retain their portable target when that issue is permanently removed.
func checkCommentReplyPurge(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Reply", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Finding", Author: "worker"})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, p.ID, "worker")
	require.NoError(t, err)
	finding, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "worker", Body: "Finding"})
	require.NoError(t, err)
	reply, creation, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Answer", ReplyToUID: finding.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, target.ID, "worker", nil)
	require.NoError(t, err)
	comments, err := store.CommentsByIssue(ctx, source.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, reply.UID, comments[0].UID)
	require.Equal(t, finding.UID, comments[0].ReplyToUID)
	events, err := store.EventsByUIDs(ctx, p.ID, []string{creation.UID})
	require.NoError(t, err)
	require.Len(t, events, 1, "purging a target must retain the source reply event")
	require.Nil(t, events[0].RelatedIssueID)
	require.Equal(t, &target.UID, events[0].RelatedIssueUID)
	require.Equal(t, creation.ContentHash, events[0].ContentHash)
	require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
	comments, err = store.CommentsByIssue(ctx, source.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1, "reconciliation must retain the reply after target purge")
	require.Equal(t, reply.UID, comments[0].UID)
	require.Equal(t, finding.UID, comments[0].ReplyToUID)
	require.Equal(t, reply.Author, comments[0].Author)
	return nil
}

func checkCommentReplyReplay(t *testing.T, target db.Storage, backend Backend) error {
	ctx := t.Context()
	source := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	p, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	first, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Reply", Author: "worker"})
	require.NoError(t, err)
	second, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Finding", Author: "worker"})
	require.NoError(t, err)
	finding, _, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: second.ID, Author: "worker", Body: "Finding"})
	require.NoError(t, err)
	reply, event, err := source.CreateComment(ctx, db.CreateCommentParams{IssueID: first.ID, Author: "worker", Body: "Answer", ReplyToUID: finding.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	reply, _, _, err = source.EditComment(ctx, db.EditCommentParams{IssueID: first.ID, CommentUID: reply.UID, Actor: "worker", Body: "Edited answer"})
	require.NoError(t, err)
	_, err = source.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: first.ID, Actor: "worker", Patch: map[string]jsontext.Value{"notify.worker": jsontext.Value(`{"state":"requested","re":"` + reply.UID + `"}`)}})
	require.NoError(t, err)
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{ProjectID: &p.ID, IncludeDeleted: true})
	require.NoError(t, err)
	// Put the reply row before its target: import must not require target order.
	for i, rec := range records {
		if c, ok := rec.(*db.CommentExport); ok && c.UID == reply.UID {
			records = append([]db.ImportRecord{rec}, append(records[:i], records[i+1:]...)...)
			break
		}
	}
	prepared, err := db.PrepareProjectMergeRecords(records, db.ProjectMergeOffsets{TargetProjectID: 50, Issue: 100, Comment: 200, Event: 300}, nil)
	require.NoError(t, err)
	require.NoError(t, target.ImportReplay(ctx, prepared, db.ImportOptions{}))
	got, err := target.IssueByUID(ctx, first.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, first.ID+100, got.ID)
	require.Contains(t, string(got.Metadata), reply.UID)
	comments, err := target.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, reply.UID, comments[0].UID)
	require.Equal(t, finding.UID, comments[0].ReplyToUID)
	require.Equal(t, reply.ReplyKind, comments[0].ReplyKind)
	require.NotNil(t, comments[0].EditedAt)
	require.True(t, reply.EditedAt.Equal(*comments[0].EditedAt))
	events, err := target.EventsByUIDs(ctx, 50, []string{event.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, event.Payload, events[0].Payload)
	require.Equal(t, event.ContentHash, events[0].ContentHash)
	require.Equal(t, second.ID+100, *events[0].RelatedIssueID)
	require.Equal(t, second.UID, *events[0].RelatedIssueUID)
	return nil
}
