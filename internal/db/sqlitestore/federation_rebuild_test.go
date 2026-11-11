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
	payload := fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":%q,"author":"tester","status":"open"}`, issue.UID, issue.ShortID, issue.Title)
	event := incrementalRemoteEvent(t, project, issue.UID, "issue.created", payload, 1_000_000_000_000)
	_, err = d.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{event})
	require.NoError(t, err)
	require.NoError(t, d.MaterializeFederatedProject(ctx, project.ID))
	issue, err = d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	return issue
}

func TestFederationRebuildRetriesChangedRows(t *testing.T) {
	for _, change := range []string{"priority", "comment", "label", "link"} {
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
				case "priority":
					_, err = writer.ExecContext(ctx, `UPDATE issues SET priority=1 WHERE id=?`, issue.ID)
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
			require.Equal(t, 2, attempts)
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
	for _, change := range []string{"history", "unrelated-move", "binding-group", "target-joined", "local-move", "fallback"} {
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
			if change == "local-move" {
				_, err = d.EnableProjectFederation(ctx, other.ID, "tester")
				require.NoError(t, err)
			}
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
			if change == "local-move" {
				_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: other.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: other.ID, HubProjectUID: other.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
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
					_, err := writer.ExecContext(ctx, `UPDATE issues SET title=? WHERE id=?`, fmt.Sprintf("out of date %d", attempts), preparedIssue.ID)
					require.NoError(t, err)
				case "binding-group":
					_, err := writer.LeaveFederationReplica(ctx, destination.ID)
					require.NoError(t, err)
				case "target-joined":
					_, err := writer.EnableProjectFederation(ctx, destination.ID, "tester")
					require.NoError(t, err)
				case "local-move":
					current, err := writer.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					_, err = writer.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: other.ID, IfMatchRev: current.Revision, Actor: "tester"})
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
			if change == "binding-group" || change == "target-joined" {
				projectID = source.ID
			}
			err = d.MaterializeFederatedProject(ctx, projectID)
			require.NoError(t, err)
			if change == "unrelated-move" {
				require.Equal(t, 1, attempts)
			} else {
				require.Equal(t, 2, attempts)
			}
			if change == "binding-group" || change == "local-move" {
				got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				if change == "local-move" {
					require.Equal(t, other.ID, got.ProjectID)
				} else {
					require.Equal(t, source.ID, got.ProjectID)
				}
				return
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

func TestMoveRebuildRemovesStaleLinksBeforeIssues(t *testing.T) {
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
	_, err = d.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author) VALUES(?,?,?,?,'blocks','tester')`, orphan.ID, issue.ID, orphan.UID, issue.UID)
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() { attempts++ }
	require.NoError(t, d.MaterializeFederatedProject(ctx, destination.ID))
	require.Equal(t, 1, attempts)
	_, err = d.IssueByUID(ctx, orphan.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	links, err := d.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Empty(t, links)
}

func TestMoveRebuildAssignsCollidingShortIDsInOrder(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "rehomed"
		if missing {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			d, err := Open(ctx, filepath.Join(t.TempDir(), "spoke.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Close()) })
			source, err := d.CreateProject(ctx, "source")
			require.NoError(t, err)
			destination, err := d.CreateProject(ctx, "destination")
			require.NoError(t, err)
			otherSource := destination
			if !missing {
				otherSource, err = d.CreateProject(ctx, "other-source")
				require.NoError(t, err)
			}
			for _, project := range []db.Project{source, destination} {
				_, err = d.EnableProjectFederation(ctx, project.ID, "tester")
				require.NoError(t, err)
			}
			if !missing {
				_, err = d.EnableProjectFederation(ctx, otherSource.ID, "tester")
				require.NoError(t, err)
			}
			first, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EC", ShortIDOverride: "d4ec", Title: "first", Author: "tester"})
			require.NoError(t, err)
			second, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: otherSource.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EC", ShortIDOverride: "d4ec", Title: "second", Author: "tester"})
			require.NoError(t, err)
			if missing {
				_, err = d.ExecContext(ctx, `UPDATE events SET issue_id=NULL WHERE issue_id=?`, second.ID)
				require.NoError(t, err)
				_, err = d.ExecContext(ctx, `DELETE FROM issues WHERE id=?`, second.ID)
				require.NoError(t, err)
			}
			type entry struct {
				issue  db.Issue
				source db.Project
			}
			entries := []entry{{first, source}}
			if !missing {
				entries = append(entries, entry{second, otherSource})
			}
			for _, entry := range entries {
				if !missing {
					unapplyMove(t, d, entry.issue, entry.source, destination, true)
					continue
				}
				_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: entry.issue.ID, FromProjectID: entry.source.ID, ToProjectID: destination.ID, IfMatchRev: entry.issue.Revision, Actor: "tester"})
				require.NoError(t, err)
				_, err = d.ExecContext(ctx, `UPDATE events SET issue_id=NULL WHERE issue_id=?`, entry.issue.ID)
				require.NoError(t, err)
				_, err = d.ExecContext(ctx, `DELETE FROM issues WHERE id=?`, entry.issue.ID)
				require.NoError(t, err)
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
				if !missing {
					require.Equal(t, issue.ID, got.ID)
				}
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
		})
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
}

func TestMoveRebuildPreservesLinks(t *testing.T) {
	for _, scenario := range []string{"archived", "absent-later-destination", "former-source", "former-source-changed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			d, source, issue := incrementalTestStore(t)
			destination, err := d.CreateProject(ctx, "destination")
			require.NoError(t, err)
			_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
			require.NoError(t, err)
			peerProject := destination
			formerSource := scenario == "former-source" || scenario == "former-source-changed"
			if formerSource {
				peerProject = source
			}
			peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peerProject.ID, Title: "peer", Author: "tester"})
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
			if formerSource {
				oldLink := link(d, "blocks")
				link(d, "related")
				hub := cloneIncrementalStore(t, d)
				var remote []db.RemoteEvent
				for i, item := range []db.Issue{issue, peer} {
					moved := move(hub, item, source, destination)
					remote = append(remote, incrementalRemoteEvent(t, destination, item.UID, "issue.moved", string(moved.Event.Payload), 9_000_000_000_000+int64(i)))
				}
				unlinked, err := hub.DeleteLinkAndEvent(ctx, oldLink, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "tester"})
				require.NoError(t, err)
				remote = append(remote, incrementalRemoteEvent(t, destination, issue.UID, "issue.unlinked", unlinked.Payload, 9_000_000_000_002))
				for _, project := range []db.Project{source, destination} {
					_, err = d.ExecContext(ctx, `UPDATE federation_bindings SET role='spoke',hub_url='https://example.com',hub_project_id=? WHERE project_id=?`, project.ID, project.ID)
					require.NoError(t, err)
				}
				_, err = d.LeaveFederationReplica(ctx, source.ID)
				require.NoError(t, err)
				_, err = d.InsertRemoteEvents(ctx, destination.ID, remote)
				require.NoError(t, err)
			} else {
				move(d, issue, source, destination)
				link(d, "blocks")
				if scenario == "archived" {
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
					move(d, peer, destination, last)
					_, err = d.ExecContext(ctx, `DELETE FROM links WHERE from_issue_id=? OR to_issue_id=?`, peer.ID, peer.ID)
					require.NoError(t, err)
					_, err = d.ExecContext(ctx, `UPDATE events SET issue_id=NULL WHERE issue_id=?`, peer.ID)
					require.NoError(t, err)
					_, err = d.ExecContext(ctx, `UPDATE events SET related_issue_id=NULL WHERE related_issue_id=?`, peer.ID)
					require.NoError(t, err)
					_, err = d.ExecContext(ctx, `DELETE FROM issues WHERE id=?`, peer.ID)
					require.NoError(t, err)
					_, err = d.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, source.ID, issue.ShortID, issue.ID)
					require.NoError(t, err)
					projectID = source.ID
				}
			}
			locked := cloneIncrementalStore(t, d)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if scenario == "former-source-changed" && attempts == 1 {
					_, err := d.ExecContext(ctx, `DELETE FROM links WHERE type='related' AND from_issue_id=?`, issue.ID)
					require.NoError(t, err)
					_, err = locked.ExecContext(ctx, `DELETE FROM links WHERE type='related' AND from_issue_id=?`, issue.ID)
					require.NoError(t, err)
				}
			}
			require.NoError(t, d.MaterializeFederatedProject(ctx, projectID))
			require.NoError(t, locked.materializeFederatedProject(ctx, projectID))
			if scenario == "former-source-changed" {
				require.Equal(t, 2, attempts)
			} else {
				require.Equal(t, 1, attempts)
			}
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
				require.Empty(t, want)
				require.Empty(t, got)
			} else {
				require.Len(t, want, 1)
				if formerSource {
					require.Equal(t, "related", want[0].Type)
				}
				if scenario == "former-source-changed" {
					// Each rebuild recreates the deleted link at its own time.
					require.Len(t, got, 1)
					want[0].CreatedAt = got[0].CreatedAt
				}
				require.Equal(t, want, got)
			}
		})
	}
}
