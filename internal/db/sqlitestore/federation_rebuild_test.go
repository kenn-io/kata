package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestMoveRebuildLeavesWriterAvailableDuringPreparation(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "spoke.db")
	d, err := Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	otherHandle, err := Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherHandle.Close()) })
	p, err := d.CreateProject(ctx, "source")
	require.NoError(t, err)
	destination, err := d.CreateProject(ctx, "destination")
	require.NoError(t, err)
	local, err := d.CreateProject(ctx, "local")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "work", Author: "tester", Labels: []string{"label"}})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "comment"})
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, p.ID, "tester")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
	require.NoError(t, err)
	moved, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: p.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	// The spoke has received the move event but has yet to move its issue row.
	_, err = d.ExecContext(ctx, `UPDATE issues SET project_id=?, short_id=? WHERE id=?`, p.ID, issue.ShortID, issue.ID)
	require.NoError(t, err)
	update := incrementalRemoteEvent(t, destination, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"updated"}`, 9_000_000_000_000)
	_, err = d.InsertRemoteEvents(ctx, destination.ID, []db.RemoteEvent{update})
	require.NoError(t, err)
	prepared := 0
	d.federationIngestPrepared = func() {
		prepared++
		if prepared != 1 {
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, _, err := otherHandle.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: local.ID, Title: "concurrent", Author: "tester"})
		require.NoError(t, err, "create must commit while the rebuild prepares")
	}
	require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
	require.Positive(t, prepared)
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, moved.Issue.ID, got.ID)
	require.Equal(t, destination.ID, got.ProjectID)
	require.Equal(t, "updated", got.Title)
	comments, err := d.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "comment", comments[0].Body)
	labels, err := d.LabelsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	require.Equal(t, "label", labels[0].Label)
}

func TestMoveRebuildRetriesChangedSnapshot(t *testing.T) {
	for _, change := range []string{"row", "history", "binding", "unrelated", "fallback"} {
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
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts > 1 && change != "fallback" {
					return
				}
				switch change {
				case "row", "fallback":
					_, err := d.ExecContext(ctx, `UPDATE issues SET title='out of date' WHERE id=?`, preparedIssue.ID)
					require.NoError(t, err)
				case "history":
					event := incrementalRemoteEvent(t, source, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"later edit"}`, 9_000_000_000_000)
					_, err := d.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{event})
					require.NoError(t, err)
				case "binding":
					_, err := d.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0 WHERE project_id=?`, source.ID)
					require.NoError(t, err)
				case "unrelated":
					_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "concurrent", Author: "tester"})
					require.NoError(t, err)
				}
			}
			require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
			if change == "unrelated" {
				require.Equal(t, 1, attempts)
			} else {
				require.Equal(t, 2, attempts)
			}
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, destination.ID, got.ProjectID)
			if change == "history" {
				require.Equal(t, "later edit", got.Title)
			} else {
				require.Equal(t, "work", got.Title)
			}
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

func TestMoveRebuildRetriesNewMove(t *testing.T) {
	ctx := t.Context()
	d, source, first := incrementalTestStore(t)
	second, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	destination, err := d.CreateProject(ctx, "destination")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
	require.NoError(t, err)
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: first.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: first.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, source.ID, first.ShortID, first.ID)
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts != 1 {
			return
		}
		_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: second.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: second.Revision, Actor: "tester"})
		require.NoError(t, err)
	}
	require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
	require.Equal(t, 2, attempts)
	for _, issue := range []db.Issue{first, second} {
		got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, destination.ID, got.ProjectID)
	}
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
	for _, entry := range []struct {
		issue  db.Issue
		source db.Project
	}{{first, firstSource}, {second, secondSource}} {
		_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: entry.issue.ID, FromProjectID: entry.source.ID, ToProjectID: destination.ID, IfMatchRev: entry.issue.Revision, Actor: "tester"})
		require.NoError(t, err)
	}
	for _, entry := range []struct {
		issue  db.Issue
		source db.Project
	}{{first, firstSource}, {second, secondSource}} {
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

func TestMoveRebuildRetriesDetachedDestination(t *testing.T) {
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
	_, err = d.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0 WHERE project_id=?`, destination.ID)
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts != 1 {
			return
		}
		_, err := d.ExecContext(ctx, `DELETE FROM federation_bindings WHERE project_id=?`, destination.ID)
		require.NoError(t, err)
	}
	require.NoError(t, d.MaterializeFederatedProject(ctx, source.ID))
	require.Equal(t, 2, attempts)
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}
