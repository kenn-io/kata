package sqlitestore

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestMoveRebuildPreparesLargeIssueScope(t *testing.T) {
	ctx := t.Context()
	d, project, first := incrementalTestStore(t)
	second, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	third, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "third", Author: "tester"})
	require.NoError(t, err)
	outside, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "outside", Author: "tester"})
	require.NoError(t, err)
	for _, issue := range []db.Issue{first, second, third, outside} {
		_, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: issue.Title})
		require.NoError(t, err)
		_, _, err = d.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: issue.Title, Actor: "tester"})
		require.NoError(t, err)
	}
	scope := make([]string, 40_001)
	for i := range scope {
		scope[i] = fmt.Sprintf("absent-%05d", i)
	}
	scope[0], scope[500], scope[len(scope)-1] = first.UID, second.UID, third.UID
	scope[501] = first.UID
	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	m := federationMaterialization{rowFingerprint: &federationRowsFingerprint{}}
	require.NoError(t, m.prepareRows(ctx, tx, project.ID, scope))
	small := federationMaterialization{rowFingerprint: &federationRowsFingerprint{}}
	require.NoError(t, small.prepareRows(ctx, tx, project.ID, []string{first.UID, second.UID, third.UID}))
	require.Equal(t, small.rowFingerprint.sum(), m.rowFingerprint.sum())
	require.Len(t, m.issueRows, 3)
	require.Len(t, m.commentRows, 3)
	require.Len(t, m.labelRows, 3)
	comments := make(map[int64]string, len(m.commentRows))
	for _, comment := range m.commentRows {
		comments[comment.issueID] = comment.body
	}
	for _, issue := range []db.Issue{first, second, third} {
		require.Equal(t, issue.ID, m.issueRows[issue.UID].id)
		require.Equal(t, issue.Title, comments[issue.ID])
		require.Contains(t, m.labelRows, federatedLabelKey{issueID: issue.ID, label: issue.Title})
	}
	require.NotContains(t, m.issueRows, outside.UID)
}

func TestMoveRebuildLeavesWriterAvailableDuringPreparation(t *testing.T) {
	for _, name := range []string{"move-free", "moved"} {
		t.Run(name, func(t *testing.T) {
			moved := name == "moved"
			ctx := t.Context()
			d, project, issue := incrementalTestStore(t)
			local, err := d.CreateProject(ctx, "local")
			require.NoError(t, err)
			writer, err := Open(ctx, d.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, writer.Close()) })
			if moved {
				destination, err := d.CreateProject(ctx, "destination")
				require.NoError(t, err)
				_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
				require.NoError(t, err)
				_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: project.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
				require.NoError(t, err)
				_, err = d.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, project.ID, issue.ShortID, issue.ID)
				require.NoError(t, err)
				project = destination
			}
			update := incrementalRemoteEvent(t, project, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"updated"}`, 9_000_000_000_000)
			_, err = d.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{update})
			require.NoError(t, err)
			prepared := false
			d.federationIngestPrepared = func() {
				if prepared {
					return
				}
				prepared = true
				writeCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				_, _, err := writer.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: local.ID, Title: "concurrent", Author: "tester"})
				require.NoError(t, err, "create must commit while the rebuild prepares")
			}
			require.NoError(t, d.MaterializeFederatedProject(ctx, project.ID))
			require.True(t, prepared)
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, project.ID, got.ProjectID)
			require.Equal(t, "updated", got.Title)
		})
	}
}

func TestMoveRebuildRetriesChangedSnapshot(t *testing.T) {
	for _, change := range []string{"history", "binding", "unrelated", "fallback", "move", "destination-binding"} {
		t.Run(change, func(t *testing.T) {
			ctx := t.Context()
			d, source, issue := incrementalTestStore(t)
			destination, err := d.CreateProject(ctx, "destination")
			require.NoError(t, err)
			_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
			require.NoError(t, err)
			preparedIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: destination.ID, Title: "prepared", Author: "tester"})
			require.NoError(t, err)
			other, err := d.CreateProject(ctx, "unrelated")
			require.NoError(t, err)
			_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
			require.NoError(t, err)
			_, err = d.ExecContext(ctx, `UPDATE issues SET project_id=?, short_id=? WHERE id=?`, source.ID, issue.ShortID, issue.ID)
			require.NoError(t, err)
			if change == "destination-binding" {
				_, err = d.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0 WHERE project_id=?`, destination.ID)
				require.NoError(t, err)
			}
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts > 1 && change != "fallback" {
					return
				}
				switch change {
				case "fallback":
					_, err := d.ExecContext(ctx, `UPDATE issues SET title='out of date' WHERE id=?`, preparedIssue.ID)
					require.NoError(t, err)
				case "history":
					event := incrementalRemoteEvent(t, source, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"later edit"}`, 9_000_000_000_000)
					_, err := d.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{event})
					require.NoError(t, err)
				case "binding":
					_, err := d.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0 WHERE project_id=?`, source.ID)
					require.NoError(t, err)
				case "move":
					_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: preparedIssue.ID, FromProjectID: destination.ID, ToProjectID: source.ID, IfMatchRev: preparedIssue.Revision, Actor: "tester"})
					require.NoError(t, err)
				case "destination-binding":
					_, err := d.ExecContext(ctx, `DELETE FROM federation_bindings WHERE project_id=?`, destination.ID)
					require.NoError(t, err)
				case "unrelated":
					_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "concurrent", Author: "tester"})
					require.NoError(t, err)
				}
			}
			projectID := destination.ID
			if change == "destination-binding" {
				projectID = source.ID
			}
			require.NoError(t, d.MaterializeFederatedProject(ctx, projectID))
			if change == "unrelated" {
				require.Equal(t, 1, attempts)
			} else {
				require.Equal(t, 2, attempts)
			}
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, projectID, got.ProjectID)
			if change == "move" {
				second, err := d.IssueByUID(ctx, preparedIssue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, source.ID, second.ProjectID)
			}
			if change == "history" {
				require.Equal(t, "later edit", got.Title)
			} else {
				require.Equal(t, "work", got.Title)
			}
		})
	}
}

func TestMoveRebuildRetriesInPlaceRowChanges(t *testing.T) {
	for _, change := range []string{"issue", "comment", "label", "link"} {
		t.Run(change, func(t *testing.T) {
			ctx := t.Context()
			d, source, issue := incrementalTestStore(t)
			destination, err := d.CreateProject(ctx, "destination")
			require.NoError(t, err)
			_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
			require.NoError(t, err)
			peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: destination.ID, Title: "peer", Author: "tester"})
			require.NoError(t, err)
			comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: peer.ID, Author: "tester", Body: "original"})
			require.NoError(t, err)
			_, _, err = d.AddLabelAndEvent(ctx, peer.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "original", Actor: "tester"})
			require.NoError(t, err)
			other, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: destination.ID, Title: "other", Author: "tester"})
			require.NoError(t, err)
			_, _, err = d.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: peer.ID, ToIssueID: other.ID, Type: "related", Author: "tester"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: peer.ID, FromShortID: peer.ShortID, FromUID: peer.UID, ToShortID: other.ShortID, ToUID: other.UID, Actor: "tester"})
			require.NoError(t, err)
			_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
			require.NoError(t, err)
			_, err = d.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, source.ID, issue.ShortID, issue.ID)
			require.NoError(t, err)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts != 1 {
					return
				}
				var err error
				switch change {
				case "issue":
					_, err = d.ExecContext(ctx, `UPDATE issues SET title='changed' WHERE id=?`, peer.ID)
				case "comment":
					_, err = d.ExecContext(ctx, `UPDATE comments SET body='changed' WHERE id=?`, comment.ID)
				case "label":
					_, err = d.ExecContext(ctx, `UPDATE issue_labels SET label='changed' WHERE issue_id=?`, peer.ID)
				case "link":
					_, err = d.ExecContext(ctx, `UPDATE links SET author='changed' WHERE from_issue_id=? AND to_issue_id=?`, peer.ID, other.ID)
				}
				require.NoError(t, err)
			}
			require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
			require.Equal(t, 2, attempts)
			got, err := d.IssueByUID(ctx, peer.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "peer", got.Title)
			comments, err := d.CommentsByIssue(ctx, peer.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "original", comments[0].Body)
			labels, err := d.LabelsByIssue(ctx, peer.ID)
			require.NoError(t, err)
			require.Len(t, labels, 1)
			require.Equal(t, "original", labels[0].Label)
			links, err := d.LinksByIssue(ctx, peer.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, "tester", links[0].Author)
		})
	}
}

func TestMoveRebuildRemovesStaleLinksBeforeIssues(t *testing.T) {
	ctx := t.Context()
	d, source, issue := incrementalTestStore(t)
	destination, err := d.CreateProject(ctx, "destination")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
	require.NoError(t, err)
	orphan, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: destination.ID, Title: "orphan", Author: "tester"})
	require.NoError(t, err)
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, source.ID, issue.ShortID, issue.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `DELETE FROM events WHERE issue_id=?`, orphan.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author) VALUES(?,?,?,?,'blocks','tester')`, orphan.ID, issue.ID, orphan.UID, issue.UID)
	require.NoError(t, err)
	require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
	_, err = d.IssueByUID(ctx, orphan.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	links, err := d.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Empty(t, links)
}

func TestMoveRebuildAssignsCollidingShortIDsInOrder(t *testing.T) {
	ctx := t.Context()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "spoke.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	firstSource, err := d.CreateProject(ctx, "first-source")
	require.NoError(t, err)
	secondSource, err := d.CreateProject(ctx, "second-source")
	require.NoError(t, err)
	destination, err := d.CreateProject(ctx, "destination")
	require.NoError(t, err)
	for _, project := range []db.Project{firstSource, secondSource, destination} {
		_, err := d.EnableProjectFederation(ctx, project.ID, "tester")
		require.NoError(t, err)
	}
	first, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: firstSource.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EC", ShortIDOverride: "d4ec", Title: "first", Author: "tester"})
	require.NoError(t, err)
	second, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: secondSource.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EC", ShortIDOverride: "d4ec", Title: "second", Author: "tester"})
	require.NoError(t, err)
	entries := []struct {
		issue  db.Issue
		source db.Project
	}{{first, firstSource}, {second, secondSource}}
	for _, entry := range entries {
		_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: entry.issue.ID, FromProjectID: entry.source.ID, ToProjectID: destination.ID, IfMatchRev: entry.issue.Revision, Actor: "tester"})
		require.NoError(t, err)
	}
	for _, entry := range entries {
		_, err := d.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, entry.source.ID, entry.issue.ShortID, entry.issue.ID)
		require.NoError(t, err)
	}
	require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
	a, err := d.IssueByUID(ctx, first.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	b, err := d.IssueByUID(ctx, second.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, first.ID, a.ID)
	require.Equal(t, second.ID, b.ID)
	require.Equal(t, destination.ID, a.ProjectID)
	require.Equal(t, destination.ID, b.ProjectID)
	require.Equal(t, "d4ec", a.ShortID)
	require.NotEqual(t, a.ShortID, b.ShortID)
	require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
	again, err := d.IssueByUID(ctx, second.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, b.ShortID, again.ShortID)
}

func TestMoveRebuildPrunesDetachedDestinationSourceRow(t *testing.T) {
	ctx := t.Context()
	d, source, issue := incrementalTestStore(t)
	destination, err := d.CreateProject(ctx, "destination")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
	require.NoError(t, err)
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, source.ID, issue.ShortID, issue.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE events SET issue_id=NULL WHERE issue_id=?`, issue.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE events SET related_issue_id=NULL WHERE related_issue_id=?`, issue.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `DELETE FROM federation_bindings WHERE project_id=?`, destination.ID)
	require.NoError(t, err)
	require.NoError(t, d.MaterializeFederatedProject(ctx, source.ID))
	_, err = d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestMoveRebuildDoesNotResurrectArchivedDestination(t *testing.T) {
	ctx := t.Context()
	prepared, source, issue := incrementalTestStore(t)
	destination, err := prepared.CreateProject(ctx, "destination")
	require.NoError(t, err)
	_, err = prepared.EnableProjectFederation(ctx, destination.ID, "tester")
	require.NoError(t, err)
	move, err := prepared.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, err = prepared.ExecContext(ctx, `DELETE FROM events WHERE id=?`, move.EventID)
	require.NoError(t, err)
	_, err = prepared.ExecContext(ctx, `UPDATE events SET issue_id=NULL WHERE issue_id=?`, issue.ID)
	require.NoError(t, err)
	_, err = prepared.ExecContext(ctx, `DELETE FROM issues WHERE id=?`, issue.ID)
	require.NoError(t, err)
	_, err = prepared.ExecContext(ctx, `UPDATE federation_bindings SET role='spoke',hub_url='https://example.com',hub_project_id=1 WHERE project_id=?`, source.ID)
	require.NoError(t, err)
	remoteMove := incrementalRemoteEvent(t, destination, issue.UID, "issue.moved", string(move.Event.Payload), 9_000_000_000_000)
	_, err = prepared.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{remoteMove})
	require.NoError(t, err)
	_, _, err = prepared.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: destination.ID, Actor: "tester"})
	require.NoError(t, err)
	copyPath := filepath.Join(t.TempDir(), "locked.db")
	_, err = prepared.ExecContext(ctx, `VACUUM INTO ?`, copyPath)
	require.NoError(t, err)
	locked, err := Open(ctx, copyPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, locked.Close()) })
	require.NoError(t, locked.materializeFederatedProject(ctx, destination.ID))
	_, err = locked.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.NoError(t, prepared.MaterializeFederatedProject(ctx, destination.ID))
	_, err = prepared.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestMoveRebuildHandlesInvalidUTF8Rows(t *testing.T) {
	for _, field := range []string{"issue", "comment"} {
		t.Run(field, func(t *testing.T) {
			ctx := t.Context()
			d, project, issue := incrementalTestStore(t)
			if field == "issue" {
				_, err := d.ExecContext(ctx, `UPDATE issues SET title=CAST(X'80' AS TEXT) WHERE id=?`, issue.ID)
				require.NoError(t, err)
			} else {
				_, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "comment"})
				require.NoError(t, err)
				_, err = d.ExecContext(ctx, `UPDATE comments SET body=CAST(X'80' AS TEXT) WHERE issue_id=?`, issue.ID)
				require.NoError(t, err)
			}
			require.NoError(t, d.MaterializeFederatedProject(ctx, project.ID))
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "work", got.Title)
			if field == "comment" {
				comments, err := d.CommentsByIssue(ctx, issue.ID)
				require.NoError(t, err)
				require.Len(t, comments, 1)
				require.Equal(t, "comment", comments[0].Body)
			}
		})
	}
}

func TestMoveRebuildRetriesChangedInvalidUTF8(t *testing.T) {
	ctx := t.Context()
	d, project, issue := incrementalTestStore(t)
	_, err := d.ExecContext(ctx, `UPDATE issues SET title=CAST(X'80' AS TEXT) WHERE id=?`, issue.ID)
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts == 1 {
			_, err := d.ExecContext(ctx, `UPDATE issues SET title=CAST(X'81' AS TEXT) WHERE id=?`, issue.ID)
			require.NoError(t, err)
		}
	}
	require.NoError(t, d.MaterializeFederatedProject(ctx, project.ID))
	require.Equal(t, 2, attempts)
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "work", got.Title)
}

func TestMoveRebuildMatchesLockedRebuild(t *testing.T) {
	ctx := t.Context()
	prepared, source, issue := incrementalTestStore(t)
	destination, err := prepared.CreateProject(ctx, "destination")
	require.NoError(t, err)
	_, err = prepared.EnableProjectFederation(ctx, destination.ID, "tester")
	require.NoError(t, err)
	bystander, err := prepared.CreateProject(ctx, "bystander")
	require.NoError(t, err)
	_, err = prepared.EnableProjectFederation(ctx, bystander.ID, "tester")
	require.NoError(t, err)
	peer, _, err := prepared.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "peer", Author: "tester"})
	require.NoError(t, err)
	second, _, err := prepared.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "second move", Author: "tester"})
	require.NoError(t, err)
	other, _, err := prepared.CreateIssue(ctx, db.CreateIssueParams{ProjectID: bystander.ID, Title: "bystander issue", Author: "tester"})
	require.NoError(t, err)
	_, _, err = prepared.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "retained comment"})
	require.NoError(t, err)
	_, _, err = prepared.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "retained", Actor: "tester"})
	require.NoError(t, err)
	for _, item := range []db.Issue{second, peer, other} {
		_, _, err = prepared.CreateComment(ctx, db.CreateCommentParams{IssueID: item.ID, Author: "tester", Body: item.Title})
		require.NoError(t, err)
		_, _, err = prepared.AddLabelAndEvent(ctx, item.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "retained", Actor: "tester"})
		require.NoError(t, err)
	}
	_, _, err = prepared.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "related", Author: "tester"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "tester"})
	require.NoError(t, err)
	_, _, err = prepared.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: second.ID, ToIssueID: other.ID, Type: "related", Author: "tester"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: second.ID, FromShortID: second.ShortID, FromUID: second.UID, ToShortID: other.ShortID, ToUID: other.UID, Actor: "tester"})
	require.NoError(t, err)
	_, err = prepared.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, err = prepared.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: second.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: second.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, err = prepared.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, source.ID, issue.ShortID, issue.ID)
	require.NoError(t, err)
	_, err = prepared.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, source.ID, second.ShortID, second.ID)
	require.NoError(t, err)
	update := incrementalRemoteEvent(t, destination, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"updated"}`, 9_000_001_000_002)
	workflow := cronPullWorkflow(t, destination, "workflow", 9_000_001_000_001)
	_, err = prepared.InsertRemoteEvents(ctx, destination.ID, []db.RemoteEvent{update, workflow})
	require.NoError(t, err)
	copyPath := filepath.Join(t.TempDir(), "locked.db")
	_, err = prepared.ExecContext(ctx, `VACUUM INTO ?`, copyPath)
	require.NoError(t, err)
	locked, err := Open(ctx, copyPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, locked.Close()) })
	require.NoError(t, prepared.MaterializeFederatedProject(ctx, destination.ID))
	require.NoError(t, locked.materializeFederatedProject(ctx, destination.ID))
	actual, err := prepared.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	want, err := locked.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, want, actual)
	actualComments, err := prepared.CommentsByIssue(ctx, actual.ID)
	require.NoError(t, err)
	wantComments, err := locked.CommentsByIssue(ctx, want.ID)
	require.NoError(t, err)
	require.Equal(t, wantComments, actualComments)
	actualLabels, err := prepared.LabelsByIssue(ctx, actual.ID)
	require.NoError(t, err)
	wantLabels, err := locked.LabelsByIssue(ctx, want.ID)
	require.NoError(t, err)
	require.Equal(t, wantLabels, actualLabels)
	actualLinks, err := prepared.LinksByIssue(ctx, actual.ID)
	require.NoError(t, err)
	wantLinks, err := locked.LinksByIssue(ctx, want.ID)
	require.NoError(t, err)
	require.Len(t, wantLinks, 1)
	require.Equal(t, wantLinks, actualLinks)
	for _, uid := range []string{peer.UID, second.UID, other.UID} {
		actualIssue, err := prepared.IssueByUID(ctx, uid, db.IncludeDeletedYes)
		require.NoError(t, err)
		wantIssue, err := locked.IssueByUID(ctx, uid, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, wantIssue, actualIssue)
		actualComments, err := prepared.CommentsByIssue(ctx, actualIssue.ID)
		require.NoError(t, err)
		wantComments, err := locked.CommentsByIssue(ctx, wantIssue.ID)
		require.NoError(t, err)
		require.Len(t, wantComments, 1)
		require.Equal(t, wantComments, actualComments)
		actualLabels, err := prepared.LabelsByIssue(ctx, actualIssue.ID)
		require.NoError(t, err)
		wantLabels, err := locked.LabelsByIssue(ctx, wantIssue.ID)
		require.NoError(t, err)
		require.Len(t, wantLabels, 1)
		require.Equal(t, wantLabels, actualLabels)
		actualLinks, err := prepared.LinksByIssue(ctx, actualIssue.ID)
		require.NoError(t, err)
		wantLinks, err := locked.LinksByIssue(ctx, wantIssue.ID)
		require.NoError(t, err)
		require.Len(t, wantLinks, 1)
		require.Equal(t, wantLinks, actualLinks)
	}
	actualWorkflow, err := (db.CronSQL{Query: prepared}).Workflow(ctx, destination.ID, "01ARZ3NDEKTSV4RRFFQ69G5FAA")
	require.NoError(t, err)
	wantWorkflow, err := (db.CronSQL{Query: locked}).Workflow(ctx, destination.ID, "01ARZ3NDEKTSV4RRFFQ69G5FAA")
	require.NoError(t, err)
	require.Equal(t, wantWorkflow, actualWorkflow)
}
