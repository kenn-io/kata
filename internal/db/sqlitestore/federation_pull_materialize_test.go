package sqlitestore

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Pulls must repair only their touched issue and agree with a complete fold,
// including older incoming clocks and payload-only issue identity. Go's built-in
// fuzzing keeps this contract independent of a third-party test dependency.
func FuzzPulledMaterializationMatchesFullFold(f *testing.F) {
	f.Add(uint16(0), false)
	f.Add(uint16(1), false)
	f.Add(uint16(100), true)
	f.Add(uint16(101), true)
	f.Fuzz(func(t *testing.T, value uint16, payloadOnly bool) {
		ctx := t.Context()
		d, p, issue := incrementalTestStore(t)
		other, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "untouched", Author: "tester", Labels: []string{"retained"}})
		require.NoError(t, err)
		_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "tester", Body: "retained note"})
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `UPDATE issues SET title='local projection' WHERE id=?`, other.ID)
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `UPDATE projects SET metadata='{"retained":true}' WHERE id=?`, p.ID)
		require.NoError(t, err)
		clock := int64(9_000_000_000_000)
		if value%2 == 0 {
			clock = 1
		}
		uid := issue.UID
		if payloadOnly {
			uid = ""
		}
		event := incrementalRemoteEvent(t, p, uid, "issue.updated", fmt.Sprintf(`{"issue_uid":%q,"title":%q}`, issue.UID, fmt.Sprintf("incoming %d", value)), clock)
		_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{event})
		require.NoError(t, err)
		// The fallback makes the same behavioral test runnable before the optional
		// SQLite fast path exists, where it must fail for refolding the sibling.
		if incremental, ok := any(d).(interface {
			MaterializeFederatedEvents(context.Context, int64, []string) error
		}); ok {
			err = incremental.MaterializeFederatedEvents(ctx, p.ID, []string{event.EventUID})
		} else {
			err = d.MaterializeFederatedProject(ctx, p.ID)
		}
		require.NoError(t, err)
		actual, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		want := "work"
		if value%2 != 0 {
			want = fmt.Sprintf("incoming %d", value)
		}
		require.Equal(t, want, actual.Title)
		sibling, err := d.IssueByUID(ctx, other.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, "local projection", sibling.Title)
		project, err := d.ProjectByID(ctx, p.ID)
		require.NoError(t, err)
		require.JSONEq(t, `{"retained":true}`, string(project.Metadata))
		labels, err := d.LabelsByIssue(ctx, other.ID)
		require.NoError(t, err)
		require.Len(t, labels, 1)
		require.Equal(t, "retained", labels[0].Label)
		require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
		full, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, full, actual)
	})
}

func TestPulledMaterializationRetriesPreparedSnapshot(t *testing.T) {
	for _, steady := range []bool{false, true} {
		t.Run(strconv.FormatBool(steady), func(t *testing.T) {
			ctx := t.Context()
			d, p, issue := incrementalTestStore(t)
			other, err := d.CreateProject(ctx, "other-project")
			require.NoError(t, err)
			ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"updated"}`, 9_000_000_000_000)
			_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
			require.NoError(t, err)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if !steady && attempts > 1 {
					return
				}
				_, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: other.ID, Actor: "tester", Patch: map[string]jsontext.Value{"write": []byte(strconv.Itoa(attempts))}})
				require.NoError(t, err, "preparation must leave the writer available")
			}
			if incremental, ok := any(d).(interface {
				MaterializeFederatedEvents(context.Context, int64, []string) error
			}); ok {
				err = incremental.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID})
			} else {
				err = d.MaterializeFederatedProject(ctx, p.ID)
			}
			require.NoError(t, err)
			require.Equal(t, 2, attempts)
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "updated", got.Title)
		})
	}
}

// Group links include endpoints outside the page's touched issue scope.
func TestPulledMaterializationRetainsUntouchedLinkEndpoints(t *testing.T) {
	ctx := t.Context()
	d, p, source := incrementalTestStore(t)
	target, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "target", Author: "tester"})
	require.NoError(t, err)
	ev := incrementalRemoteEvent(t, p, source.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, source.UID, target.UID), 9_000_000_000_000)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
	require.NoError(t, err)
	incremental, ok := any(d).(interface {
		MaterializeFederatedEvents(context.Context, int64, []string) error
	})
	require.True(t, ok)
	require.NoError(t, incremental.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
	links, err := d.LinksByIssue(ctx, source.ID)
	require.NoError(t, err)
	require.Len(t, links, 1, "untouched same-project endpoint must stay visible to the group fold")
	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
	full, err := d.LinksByIssue(ctx, source.ID)
	require.NoError(t, err)
	require.Equal(t, full, links)
}

func TestPulledMaterializationReconcilesCommentIdentityAndReplyFields(t *testing.T) {
	ctx := t.Context()
	origin, err := Open(ctx, filepath.Join(t.TempDir(), "origin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, origin.Close()) })
	target, err := Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })

	originProject, err := origin.CreateProject(ctx, "origin-project")
	require.NoError(t, err)
	originIssue, _, err := origin.CreateIssue(ctx, db.CreateIssueParams{ProjectID: originProject.ID, Title: "Task", Author: "tester"})
	require.NoError(t, err)
	originTarget, _, err := origin.CreateComment(ctx, db.CreateCommentParams{IssueID: originIssue.ID, Author: "tester", Body: "Target"})
	require.NoError(t, err)
	originReply, _, err := origin.CreateComment(ctx, db.CreateCommentParams{
		IssueID: originIssue.ID, Author: "tester", Body: "Reply", ReplyToUID: originTarget.UID, ReplyKind: "confirm",
	})
	require.NoError(t, err)
	originKindReply, _, err := origin.CreateComment(ctx, db.CreateCommentParams{
		IssueID: originIssue.ID, Author: "tester", Body: "Reply kind", ReplyToUID: originTarget.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	originEditedReply, _, err := origin.CreateComment(ctx, db.CreateCommentParams{
		IssueID: originIssue.ID, Author: "tester", Body: "Edited reply", ReplyToUID: originTarget.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)

	var imported []db.ImportRecord
	for record, err := range origin.ExportProjects(ctx, db.ExportFilter{ProjectID: &originProject.ID}) {
		require.NoError(t, err)
		exported := record
		imported = append(imported, &exported)
	}
	var issueRecord *db.IssueExport
	for record, err := range origin.ExportIssues(ctx, db.ExportFilter{ProjectID: &originProject.ID}) {
		require.NoError(t, err)
		exported := record
		issueRecord = &exported
		imported = append(imported, issueRecord)
	}
	require.NotNil(t, issueRecord)
	recordsByUID := make(map[string]*db.CommentExport, 4)
	for record, err := range origin.ExportComments(ctx, db.ExportFilter{ProjectID: &originProject.ID}) {
		require.NoError(t, err)
		exported := record
		recordsByUID[record.UID] = &exported
	}
	targetRecord := recordsByUID[originTarget.UID]
	replyRecord := recordsByUID[originReply.UID]
	kindReplyRecord := recordsByUID[originKindReply.UID]
	editedReplyRecord := recordsByUID[originEditedReply.UID]
	require.NotNil(t, targetRecord)
	require.NotNil(t, replyRecord)
	require.NotNil(t, kindReplyRecord)
	require.NotNil(t, editedReplyRecord)
	const editedAt = "2026-10-07T11:03:00.123456789Z"
	const staleEditedAt = "2026-10-07T10:03:00.123456789Z"
	lowerTargetUID := strings.ToLower(targetRecord.UID)
	targetRecord.UID = lowerTargetUID
	replyRecord.ReplyToUID = lowerTargetUID
	editedReplyRecord.EditedAt = editedAt
	imported = append(imported, targetRecord, replyRecord, kindReplyRecord, editedReplyRecord)
	require.NoError(t, target.ImportReplay(ctx, imported, db.ImportOptions{}))

	project, err := target.ProjectByUID(ctx, originProject.UID)
	require.NoError(t, err)
	issue, err := target.IssueByUID(ctx, originIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	_, err = target.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	_, err = target.ExecContext(ctx, `UPDATE comments SET reply_kind = ? WHERE id = ?`, "confirm", kindReplyRecord.ID)
	require.NoError(t, err)
	_, err = target.ExecContext(ctx, `UPDATE comments SET edited_at = ? WHERE id = ?`, staleEditedAt, editedReplyRecord.ID)
	require.NoError(t, err)

	targetPayload := fmt.Sprintf(
		"{\"comment_uid\":%q,\"author\":%q,\"body\":%q,\"created_at\":%q}",
		originTarget.UID, targetRecord.Author, targetRecord.Body, targetRecord.CreatedAt,
	)
	replyPayload := fmt.Sprintf(
		"{\"comment_uid\":%q,\"author\":%q,\"body\":%q,\"created_at\":%q,\"reply_to_uid\":%q,\"reply_kind\":\"confirm\"}",
		originReply.UID, replyRecord.Author, replyRecord.Body, replyRecord.CreatedAt, originTarget.UID,
	)
	kindReplyPayload := fmt.Sprintf(
		"{\"comment_uid\":%q,\"author\":%q,\"body\":%q,\"created_at\":%q,\"reply_to_uid\":%q,\"reply_kind\":\"reply\"}",
		originKindReply.UID, kindReplyRecord.Author, kindReplyRecord.Body, kindReplyRecord.CreatedAt, originTarget.UID,
	)
	editedReplyPayload := fmt.Sprintf(
		"{\"comment_uid\":%q,\"author\":%q,\"body\":%q,\"created_at\":%q,\"reply_to_uid\":%q,\"reply_kind\":\"reply\",\"edited_at\":%q}",
		originEditedReply.UID, editedReplyRecord.Author, editedReplyRecord.Body, editedReplyRecord.CreatedAt, originTarget.UID, editedAt,
	)
	snapshot := fmt.Sprintf(
		"{\"uid\":%q,\"title\":%q,\"author\":%q,\"status\":%q,\"created_at\":%q,\"comments\":[%s,%s,%s,%s]}",
		issueRecord.UID, issueRecord.Title, issueRecord.Author, issueRecord.Status, issueRecord.CreatedAt,
		targetPayload, replyPayload, kindReplyPayload, editedReplyPayload,
	)
	event := incrementalRemoteEvent(t, project, issue.UID, "issue.snapshot", snapshot, 9_000_000_000_000)
	_, err = target.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{event})
	require.NoError(t, err)
	require.NoError(t, target.MaterializeFederatedEvents(ctx, project.ID, []string{event.EventUID}))

	comments, err := target.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	var gotTarget, gotReply, gotKindReply, gotEditedReply *db.Comment
	for index := range comments {
		switch comments[index].ID {
		case targetRecord.ID:
			gotTarget = &comments[index]
		case replyRecord.ID:
			gotReply = &comments[index]
		case kindReplyRecord.ID:
			gotKindReply = &comments[index]
		case editedReplyRecord.ID:
			gotEditedReply = &comments[index]
		}
	}
	require.NotNil(t, gotTarget)
	require.Equal(t, originTarget.UID, gotTarget.UID, "incremental materialization must normalize the restored target UID")
	require.NotNil(t, gotReply)
	require.Equal(t, originTarget.UID, gotReply.ReplyToUID, "incremental materialization must normalize the restored reply target")
	require.Equal(t, "confirm", gotReply.ReplyKind)
	require.NotNil(t, gotKindReply)
	require.Equal(t, "reply", gotKindReply.ReplyKind, "incremental materialization must restore changed reply kinds")
	require.NotNil(t, gotEditedReply)
	require.Equal(t, "reply", gotEditedReply.ReplyKind)
	require.NotNil(t, gotEditedReply.EditedAt)
	require.Equal(t, editedAt, gotEditedReply.EditedAt.UTC().Format(time.RFC3339Nano), "incremental materialization must restore changed edit timestamps")
}
