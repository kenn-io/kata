package sqlitestore

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func unapplyMove(t *testing.T, d *Store, issue db.Issue, source, destination db.Project, joined bool) {
	t.Helper()
	ctx := t.Context()
	hub := cloneIncrementalStore(t, d)
	for _, project := range []db.Project{source, destination} {
		_, err := hub.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectUID: project.UID, Enabled: true})
		require.NoError(t, err)
	}
	move, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	for _, project := range []db.Project{source, destination} {
		if !joined && project.ID == destination.ID {
			continue
		}
		_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
		require.NoError(t, err)
	}
	event := incrementalRemoteEvent(t, destination, issue.UID, "issue.moved", string(move.Event.Payload), 9_000_000_000_000)
	route := destination.ID
	if !joined {
		route = source.ID
	}
	_, err = d.InsertRemoteEvents(ctx, route, []db.RemoteEvent{event})
	require.NoError(t, err)
}

func resetToRemoteIssue(t *testing.T, d *Store, project db.Project, issue db.Issue) db.Issue {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, d.ResetFederatedProject(ctx, project.ID, 1, 0))
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
	require.NoError(t, err)
	payload := fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":%q,"body":"","author":"tester","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`, issue.UID, issue.ShortID, issue.Title)
	event := incrementalRemoteEvent(t, project, issue.UID, "issue.created", payload, 1_000_000_000_000)
	_, err = d.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{event})
	require.NoError(t, err)
	require.NoError(t, d.MaterializeFederatedProject(ctx, project.ID))
	issue, err = d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	return issue
}

func TestFederationRebuildRetriesChangedRows(t *testing.T) {
	for _, change := range []string{"comment", "label", "link"} {
		t.Run(change, func(t *testing.T) {
			ctx := t.Context()
			d, project, issue := incrementalTestStore(t)
			peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "peer", Author: "tester"})
			require.NoError(t, err)
			comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "comment"})
			require.NoError(t, err)
			_, _, err = d.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "label", Actor: "tester"})
			require.NoError(t, err)
			_, _, err = d.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "related", Author: "tester"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "tester"})
			require.NoError(t, err)
			writer, err := Open(ctx, d.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, writer.Close()) })
			attempts := 0
			var locked *Store
			d.federationIngestPrepared = func() {
				attempts++
				if attempts != 1 {
					return
				}
				switch change {
				case "comment":
					_, err = writer.ExecContext(ctx, `UPDATE comments SET body='changed' WHERE id=?`, comment.ID)
				case "label":
					_, err = writer.ExecContext(ctx, `DELETE FROM issue_labels WHERE issue_id=?`, issue.ID)
				case "link":
					_, err = writer.ExecContext(ctx, `DELETE FROM links WHERE from_issue_id=?`, issue.ID)
				}
				require.NoError(t, err)
				locked = cloneIncrementalStore(t, d)
				require.NoError(t, locked.materializeFederatedProject(ctx, project.ID))
			}
			require.NoError(t, d.MaterializeFederatedProject(ctx, project.ID))
			if change == "link" {
				require.Equal(t, 1, attempts)
			} else {
				require.Equal(t, 2, attempts)
			}
			require.Equal(t, federationProjectionRows(t, locked, project.ID), federationProjectionRows(t, d, project.ID))
		})
	}
}

func TestFederationMaterializationPreparesLargeIssueScope(t *testing.T) {
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
	// The large scope is bound as one JSON array instead of individual parameters.
	scope := make([]string, 32_767)
	for i := range scope {
		scope[i] = fmt.Sprintf("absent-%05d", i)
	}
	scope[0], scope[500], scope[len(scope)-1] = first.UID, second.UID, third.UID
	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	m := federationMaterialization{}
	require.NoError(t, m.prepareRows(ctx, tx, project.ID, scope))
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

func TestMoveRebuildRetriesChangedSnapshot(t *testing.T) {
	for _, change := range []string{"history", "unrelated-move", "target-joined", "fallback"} {
		t.Run(change, func(t *testing.T) {
			ctx := t.Context()
			d, source, issue := incrementalTestStore(t)
			destination, err := d.CreateProject(ctx, "destination")
			require.NoError(t, err)
			if change != "target-joined" {
				_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
				require.NoError(t, err)
			}
			var preparedIssue db.Issue
			if change == "fallback" {
				preparedIssue, _, err = d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: destination.ID, Title: "prepared", Author: "tester"})
				require.NoError(t, err)
			}
			other, err := d.CreateProject(ctx, "unrelated")
			require.NoError(t, err)
			var otherDestination db.Project
			var unrelatedIssue db.Issue
			if change == "unrelated-move" {
				otherDestination, err = d.CreateProject(ctx, "other-destination")
				require.NoError(t, err)
				unrelatedIssue, _, err = d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "unrelated", Author: "tester"})
				require.NoError(t, err)
			}
			writer, err := Open(ctx, d.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, writer.Close()) })
			unapplyMove(t, d, issue, source, destination, change != "target-joined")
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts > 1 && change != "fallback" {
					return
				}
				switch change {
				case "fallback":
					_, err := writer.ExecContext(ctx, `UPDATE issues SET title=? WHERE id=?`, fmt.Sprintf("out of date %d", attempts), preparedIssue.ID)
					require.NoError(t, err)
				case "target-joined":
					_, err := writer.EnableProjectFederation(ctx, destination.ID, "tester")
					require.NoError(t, err)
				case "history":
					event := incrementalRemoteEvent(t, source, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"later edit"}`, 9_000_000_000_000)
					_, err := d.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{event})
					require.NoError(t, err)
				case "unrelated-move":
					_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: unrelatedIssue.ID, FromProjectID: other.ID, ToProjectID: otherDestination.ID, IfMatchRev: unrelatedIssue.Revision, Actor: "tester"})
					require.NoError(t, err)
				}
			}
			projectID := destination.ID
			if change == "target-joined" {
				projectID = source.ID
			}
			err = d.MaterializeFederatedProject(ctx, projectID)
			require.NoError(t, err)
			if change == "unrelated-move" {
				require.Equal(t, 1, attempts)
			} else {
				require.Equal(t, 2, attempts)
			}
			if change == "fallback" {
				repaired, err := d.IssueByUID(ctx, preparedIssue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "prepared", repaired.Title)
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

func TestMoveRebuildRetainsUnjoinedIssuesAndPrunesStaleLinks(t *testing.T) {
	ctx := t.Context()
	d, source, issue := incrementalTestStore(t)
	destination, err := d.CreateProject(ctx, "destination")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
	require.NoError(t, err)
	orphan, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: destination.ID, Title: "orphan", Author: "tester"})
	require.NoError(t, err)
	third, err := d.CreateProject(ctx, "third")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, third.ID, "tester")
	require.NoError(t, err)
	orphan = resetToRemoteIssue(t, d, destination, orphan)
	unapplyMove(t, d, issue, source, destination, true)
	unapplyMove(t, d, orphan, destination, third, true)
	_, err = d.LeaveFederationReplica(ctx, third.ID)
	require.NoError(t, err)
	from, to := orphan, issue
	if from.ID > to.ID {
		from, to = to, from
	}
	_, err = d.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author) VALUES(?,?,?,?,'related','tester')`, from.ID, to.ID, from.UID, to.UID)
	require.NoError(t, err)
	typ, payload := incrementalFuzzPayload(16, from.UID, to.UID, 1)
	_, err = d.InsertRemoteEvents(ctx, destination.ID, []db.RemoteEvent{incrementalRemoteEvent(t, destination, from.UID, typ, payload, 8_000_000_000_000)})
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() { attempts++ }
	require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
	require.Equal(t, 1, attempts)
	retained, err := d.IssueByUID(ctx, orphan.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, orphan.ID, retained.ID)
	require.Equal(t, third.ID, retained.ProjectID)
	links, err := d.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Empty(t, links)
}

func TestMoveRebuildKeepsHubEditInUnjoinedDestination(t *testing.T) {
	ctx := t.Context()
	d, source, issue := incrementalTestStore(t)
	issue = resetToRemoteIssue(t, d, source, issue)
	peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "peer", Author: "tester"})
	require.NoError(t, err)
	destination, err := d.CreateProject(ctx, "destination")
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "local comment"})
	require.NoError(t, err)
	_, err = d.AddLabel(ctx, issue.ID, "local", "tester")
	require.NoError(t, err)
	unapplyMove(t, d, issue, source, destination, false)
	var events []db.RemoteEvent
	for i, kind := range []byte{0, 5, 7, 13} {
		typ, payload := incrementalFuzzPayload(kind, issue.UID, "", 42)
		events = append(events, incrementalRemoteEvent(t, source, issue.UID, typ, payload, 8_000_000_000_000+int64(i)))
	}
	typ, payload := incrementalFuzzPayload(15, issue.UID, peer.UID, 42)
	events = append(events, incrementalRemoteEvent(t, source, issue.UID, typ, payload, 8_000_000_000_010))
	_, err = d.InsertRemoteEvents(ctx, source.ID, events)
	require.NoError(t, err)
	locked := cloneIncrementalStore(t, d)
	attempts := 0
	d.federationIngestPrepared = func() { attempts++ }
	require.NoError(t, d.MaterializeFederatedProject(ctx, source.ID))
	require.Equal(t, 1, attempts)
	require.NoError(t, locked.materializeFederatedProject(ctx, source.ID))
	for _, store := range []*Store{d, locked} {
		got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, issue.ID, got.ID)
		require.Equal(t, destination.ID, got.ProjectID)
		require.Equal(t, "work 42", got.Title)
		require.JSONEq(t, `{"area":"value-42"}`, string(got.Metadata))
		comments, err := store.CommentsByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Len(t, comments, 2)
		require.ElementsMatch(t, []string{"local comment", "comment"}, []string{comments[0].Body, comments[1].Body})
		labels, err := store.LabelsByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Len(t, labels, 2)
		require.ElementsMatch(t, []string{"area:db", "local"}, []string{labels[0].Label, labels[1].Label})
		links, err := store.LinksByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Len(t, links, 1)
		typ, payload := "issue.commented", fmt.Sprintf(`{"issue_uid":%q,"comment_uid":"01HZNQ7VFPK1XGD8R5MABCDEF1","author":"tester","body":"delayed","created_at":"2026-05-23T12:00:00.000Z"}`, issue.UID)
		_, err = store.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{incrementalRemoteEvent(t, source, issue.UID, typ, payload, 8_000_000_000_011)})
		require.NoError(t, err)
		if store == d {
			require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID))
			require.Equal(t, 2, attempts)
		} else {
			require.NoError(t, store.materializeFederatedProject(ctx, source.ID))
		}
		comments, err = store.CommentsByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Len(t, comments, 3)
		_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: destination.ID, Actor: "tester", Force: true})
		require.NoError(t, err)
		_, _, _, err = store.RestoreProject(ctx, destination.ID, "tester")
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: destination.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: destination.ID, HubProjectUID: destination.UID, Enabled: true, Actor: "tester"})
		require.NoError(t, err)
		if store == d {
			require.NoError(t, store.MaterializeFederatedProject(ctx, destination.ID))
		} else {
			require.NoError(t, store.materializeFederatedProject(ctx, destination.ID))
		}
		comments, err = store.CommentsByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Len(t, comments, 3)
		labels, err = store.LabelsByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Len(t, labels, 2)
		links, err = store.LinksByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Len(t, links, 1)
	}
}

func TestMoveRebuildAssignsCollidingShortIDsInOrder(t *testing.T) {
	ctx := t.Context()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "spoke.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	source, err := d.CreateProject(ctx, "source")
	require.NoError(t, err)
	destination, err := d.CreateProject(ctx, "destination")
	require.NoError(t, err)
	otherSource, err := d.CreateProject(ctx, "other-source")
	require.NoError(t, err)
	for _, project := range []db.Project{source, destination} {
		_, err = d.EnableProjectFederation(ctx, project.ID, "tester")
		require.NoError(t, err)
	}
	_, err = d.EnableProjectFederation(ctx, otherSource.ID, "tester")
	require.NoError(t, err)
	first, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EC", ShortIDOverride: "d4ec", Title: "first", Author: "tester"})
	require.NoError(t, err)
	second, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: otherSource.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EC", ShortIDOverride: "d4ec", Title: "second", Author: "tester"})
	require.NoError(t, err)
	type entry struct {
		issue  db.Issue
		source db.Project
	}
	for _, entry := range []entry{{first, source}, {second, otherSource}} {
		unapplyMove(t, d, entry.issue, entry.source, destination, true)
	}
	locked := cloneIncrementalStore(t, d)
	require.NoError(t, locked.materializeFederatedProject(ctx, destination.ID))
	attempts := 0
	d.federationIngestPrepared = func() { attempts++ }
	require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
	require.Equal(t, 1, attempts)
	var shortIDs []string
	for _, issue := range []db.Issue{first, second} {
		want, err := locked.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, issue.ID, got.ID)
		require.Equal(t, want.ShortID, got.ShortID)
		require.Equal(t, destination.ID, got.ProjectID)
		shortIDs = append(shortIDs, got.ShortID)
	}
	require.Equal(t, "d4ec", shortIDs[0])
	require.NotEqual(t, shortIDs[0], shortIDs[1])
	require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
	for i, uid := range []string{first.UID, second.UID} {
		again, err := d.IssueByUID(ctx, uid, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, shortIDs[i], again.ShortID)
	}
}

func TestMoveRebuildDoesNotResurrectArchivedDestination(t *testing.T) {
	ctx := t.Context()
	prepared, source, issue := incrementalTestStore(t)
	destination, err := prepared.CreateProject(ctx, "destination")
	require.NoError(t, err)
	_, err = prepared.EnableProjectFederation(ctx, destination.ID, "tester")
	require.NoError(t, err)
	hub := cloneIncrementalStore(t, prepared)
	move, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	require.NoError(t, prepared.ResetFederatedProject(ctx, source.ID, 1, 0))
	_, err = prepared.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: source.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
	require.NoError(t, err)
	remoteMove := incrementalRemoteEvent(t, destination, issue.UID, "issue.moved", string(move.Event.Payload), 9_000_000_000_000)
	_, err = prepared.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{remoteMove})
	require.NoError(t, err)
	_, _, err = prepared.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: destination.ID, Actor: "tester"})
	require.NoError(t, err)
	attempts := 0
	prepared.federationIngestPrepared = func() { attempts++ }
	require.NoError(t, prepared.MaterializeFederatedProject(ctx, destination.ID))
	require.Equal(t, 1, attempts)
	_, err = prepared.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestMoveRebuildMatchesLockedRebuild(t *testing.T) {
	t.Run("relay link across chained project moves", func(t *testing.T) {
		ctx := t.Context()
		d, source, issue := incrementalTestStore(t)
		middle, err := d.CreateProject(ctx, "middle")
		require.NoError(t, err)
		last, err := d.CreateProject(ctx, "last")
		require.NoError(t, err)
		for _, project := range []db.Project{middle, last} {
			_, err = d.EnableProjectFederation(ctx, project.ID, "tester")
			require.NoError(t, err)
		}
		peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: middle.ID, Title: "peer", Author: "tester"})
		require.NoError(t, err)
		hub := cloneIncrementalStore(t, d)
		firstMove, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: middle.ID, IfMatchRev: issue.Revision, Actor: "tester"})
		require.NoError(t, err)
		_, linkEvent, err := hub.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "related", Author: "tester"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromUID: issue.UID, ToUID: peer.UID, Actor: "tester"})
		require.NoError(t, err)
		lastMove, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: middle.ID, ToProjectID: last.ID, IfMatchRev: peer.Revision, Actor: "tester"})
		require.NoError(t, err)
		for _, project := range []db.Project{source, middle, last} {
			_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
			require.NoError(t, err)
		}
		first := incrementalRemoteEvent(t, middle, issue.UID, "issue.moved", string(firstMove.Event.Payload), 9_000_000_000_000)
		link := incrementalRemoteEvent(t, middle, issue.UID, "issue.linked", linkEvent.Payload, 9_000_000_000_001)
		movedPeer := incrementalRemoteEvent(t, last, peer.UID, "issue.moved", string(lastMove.Event.Payload), 9_000_000_000_002)
		_, err = d.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{first})
		require.NoError(t, err)
		_, err = d.InsertRemoteEvents(ctx, middle.ID, []db.RemoteEvent{link, movedPeer})
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key) VALUES(?,?,?,?)`, middle.UID, "authority", "key", "public")
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES(?,?,?,?,?,?,?)`, middle.UID, link.EventUID, link.ContentHash, 1, 1, "key", `{"ingress_instance_uid":"remote"}`)
		require.NoError(t, err)
		locked := cloneIncrementalStore(t, d)
		attempts := 0
		d.federationIngestPrepared = func() { attempts++ }
		require.NoError(t, d.MaterializeFederatedProject(ctx, source.ID))
		require.Equal(t, 1, attempts)
		require.NoError(t, locked.materializeFederatedProject(ctx, source.ID))
		for _, project := range []db.Project{source, middle, last} {
			require.Equal(t, federationProjectionRows(t, locked, project.ID), federationProjectionRows(t, d, project.ID))
		}
		links, err := d.LinksByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Empty(t, links)
	})
	t.Run("relay link ownership filtering", func(t *testing.T) {
		ctx := t.Context()
		d, source, issue := incrementalTestStore(t)
		destination, err := d.CreateProject(ctx, "destination")
		require.NoError(t, err)
		_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
		require.NoError(t, err)
		peerUID := "01HZNQ7VFPK1XGD8R5MABC0001"
		sameUID := "01HZNQ7VFPK1XGD8R5MABC0002"
		peer := incrementalRemoteEvent(t, destination, peerUID, "issue.snapshot", fmt.Sprintf(`{"uid":%q,"short_id":"0001","title":"peer","body":"","author":"tester","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","links":[{"type":"related","to_issue_uid":%q,"author":"tester"}]}`, peerUID, sameUID), 9_000_000_000_000)
		same := incrementalRemoteEvent(t, destination, sameUID, "issue.snapshot", fmt.Sprintf(`{"uid":%q,"short_id":"0002","title":"same","body":"","author":"tester","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`, sameUID), 9_000_000_000_001)
		link := incrementalRemoteEvent(t, source, issue.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, issue.UID, peerUID), 9_000_000_000_002)
		_, err = d.InsertRemoteEvents(ctx, destination.ID, []db.RemoteEvent{peer, same})
		require.NoError(t, err)
		_, err = d.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{link})
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key) VALUES(?,?,?,?)`, source.UID, "authority", "key", "public")
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES(?,?,?,?,?,?,?)`, source.UID, link.EventUID, link.ContentHash, 1, 1, "key", `{"ingress_instance_uid":"remote"}`)
		require.NoError(t, err)
		locked := cloneIncrementalStore(t, d)
		attempts := 0
		d.federationIngestPrepared = func() { attempts++ }
		require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
		require.Equal(t, 1, attempts)
		require.NoError(t, locked.materializeFederatedProject(ctx, destination.ID))
		for _, project := range []db.Project{source, destination} {
			require.Equal(t, federationProjectionRows(t, locked, project.ID), federationProjectionRows(t, d, project.ID))
		}
		links, err := d.LinksByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Empty(t, links)
		created, err := d.IssueByUID(ctx, peerUID, db.IncludeDeletedYes)
		require.NoError(t, err)
		links, err = d.LinksByIssue(ctx, created.ID)
		require.NoError(t, err)
		require.Len(t, links, 1)
	})
	t.Run("paused destination", func(t *testing.T) {
		ctx := t.Context()
		d, source, issue := incrementalTestStore(t)
		destination, err := d.CreateProject(ctx, "destination")
		require.NoError(t, err)
		_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
		require.NoError(t, err)
		peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "peer", Author: "tester"})
		require.NoError(t, err)
		link := incrementalRemoteEvent(t, source, issue.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, issue.UID, peer.UID), 8_000_000_000_001)
		unapplyMove(t, d, issue, source, destination, true)
		_, err = d.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{link})
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0 WHERE project_id=?`, destination.ID)
		require.NoError(t, err)
		locked := cloneIncrementalStore(t, d)
		attempts := 0
		d.federationIngestPrepared = func() { attempts++ }
		require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
		require.Equal(t, 1, attempts)
		require.NoError(t, locked.materializeFederatedProject(ctx, destination.ID))
		moved, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, destination.ID, moved.ProjectID)
		for _, project := range []db.Project{source, destination} {
			require.Equal(t, federationProjectionRows(t, locked, project.ID), federationProjectionRows(t, d, project.ID))
		}
		links, err := d.LinksByIssue(ctx, issue.ID)
		require.NoError(t, err)
		require.Empty(t, links)
	})
	t.Run("cron workflow", func(t *testing.T) {
		ctx := t.Context()
		prepared, source, issue := incrementalTestStore(t)
		destination, err := prepared.CreateProject(ctx, "destination")
		require.NoError(t, err)
		_, err = prepared.EnableProjectFederation(ctx, destination.ID, "tester")
		require.NoError(t, err)
		unapplyMove(t, prepared, issue, source, destination, true)
		update := incrementalRemoteEvent(t, destination, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"updated"}`, 9_000_001_000_002)
		workflow := cronPullWorkflow(t, destination, "workflow", 9_000_001_000_001)
		_, err = prepared.InsertRemoteEvents(ctx, destination.ID, []db.RemoteEvent{update, workflow})
		require.NoError(t, err)
		locked := cloneIncrementalStore(t, prepared)
		attempts := 0
		prepared.federationIngestPrepared = func() { attempts++ }
		require.NoError(t, prepared.MaterializeFederatedProject(ctx, destination.ID))
		require.Equal(t, 1, attempts)
		require.NoError(t, locked.materializeFederatedProject(ctx, destination.ID))
		for _, project := range []db.Project{source, destination} {
			require.Equal(t, federationProjectionRows(t, locked, project.ID), federationProjectionRows(t, prepared, project.ID))
		}
		actualWorkflow, err := (db.CronSQL{Query: prepared}).Workflow(ctx, destination.ID, "01ARZ3NDEKTSV4RRFFQ69G5FAA")
		require.NoError(t, err)
		wantWorkflow, err := (db.CronSQL{Query: locked}).Workflow(ctx, destination.ID, "01ARZ3NDEKTSV4RRFFQ69G5FAA")
		require.NoError(t, err)
		require.Equal(t, wantWorkflow, actualWorkflow)
	})
}

func TestMoveRebuildPreservesLinks(t *testing.T) {
	for _, scenario := range []string{"archived", "absent-later-destination"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			d, source, issue := incrementalTestStore(t)
			destination, err := d.CreateProject(ctx, "destination")
			require.NoError(t, err)
			_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
			require.NoError(t, err)
			peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: destination.ID, Title: "peer", Author: "tester"})
			require.NoError(t, err)
			link := func(store *Store, typ string) db.Link {
				link, _, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: typ, Author: "tester"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "tester"})
				require.NoError(t, err)
				return link
			}
			move := func(store *Store, item db.Issue, from, to db.Project) db.MoveIssueProjectOut {
				result, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: item.ID, FromProjectID: from.ID, ToProjectID: to.ID, IfMatchRev: item.Revision, Actor: "tester"})
				require.NoError(t, err)
				return result
			}
			projectID := destination.ID
			if scenario == "archived" {
				move(d, issue, source, destination)
				link(d, "blocks")
				for _, id := range []int64{issue.ID, peer.ID} {
					_, _, _, err = d.CloseIssue(ctx, id, "done", "tester", "finished", nil)
					require.NoError(t, err)
				}
				_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: destination.ID, Actor: "tester"})
				require.NoError(t, err)
			} else {
				last, err := d.CreateProject(ctx, "last")
				require.NoError(t, err)
				_, err = d.EnableProjectFederation(ctx, last.ID, "tester")
				require.NoError(t, err)
				hub := cloneIncrementalStore(t, d)
				firstMove := move(hub, issue, source, destination)
				_, event, err := hub.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "tester"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromUID: issue.UID, ToUID: peer.UID, Actor: "tester"})
				require.NoError(t, err)
				lastMove := move(hub, peer, destination, last)
				for _, project := range []db.Project{source, destination, last} {
					_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
					require.NoError(t, err)
				}
				require.NoError(t, d.ResetFederatedProject(ctx, destination.ID, 1, 0))
				_, err = d.IssueByUID(ctx, peer.UID, db.IncludeDeletedYes)
				require.ErrorIs(t, err, db.ErrNotFound)
				remote := []db.RemoteEvent{
					incrementalRemoteEvent(t, destination, issue.UID, "issue.moved", string(firstMove.Event.Payload), 9_000_000_000_000),
					incrementalRemoteEvent(t, destination, issue.UID, "issue.linked", event.Payload, 9_000_000_000_001),
					incrementalRemoteEvent(t, last, peer.UID, "issue.moved", string(lastMove.Event.Payload), 9_000_000_000_002),
				}
				_, err = d.InsertRemoteEvents(ctx, source.ID, remote)
				require.NoError(t, err)
				projectID = source.ID
			}
			locked := cloneIncrementalStore(t, d)
			attempts := 0
			d.federationIngestPrepared = func() { attempts++ }
			require.NoError(t, d.MaterializeFederatedProject(ctx, projectID))
			require.NoError(t, locked.materializeFederatedProject(ctx, projectID))
			require.Equal(t, 1, attempts)
			if scenario == "archived" {
				_, _, _, err = locked.RestoreProject(ctx, destination.ID, "tester")
				require.NoError(t, err)
				_, _, _, err = d.RestoreProject(ctx, destination.ID, "tester")
				require.NoError(t, err)
			}
			want, err := locked.LinksByIssue(ctx, issue.ID)
			require.NoError(t, err)
			got, err := d.LinksByIssue(ctx, issue.ID)
			require.NoError(t, err)
			if scenario == "absent-later-destination" {
				moved, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, destination.ID, moved.ProjectID)
				require.Empty(t, want)
				require.Empty(t, got)
			} else {
				require.Len(t, want, 1)
				require.Equal(t, want, got)
			}
		})
	}
}
