package dbtest

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
)

func checkSelectedCommentGraph(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "worker", Title: "Scope root"})
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "worker", Title: "Selected issue", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "worker", Title: "Other issue"})
	require.NoError(t, err)
	target, _, err := store.CreateComment(teammate.WithRequest(ctx, "writer"), db.CreateCommentParams{
		IssueID: other.ID, Author: "finder", Teammate: "route", Body: "Finding",
	})
	require.NoError(t, err)
	reply, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Answer", ReplyToUID: target.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	incoming, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "reviewer", Body: "Incoming evidence", ReplyToUID: reply.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "worker", Body: "Unrelated response", ReplyToUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", ReplyKind: "reply"})
	require.NoError(t, err)
	query := db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: source.ID}
	graph, err := store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Len(t, graph.Comments, 3)
	bodies := map[string]string{}
	for _, record := range graph.Comments {
		bodies[record.Comment.UID] = record.Comment.Body
	}
	require.Equal(t, map[string]string{target.UID: "Finding", reply.UID: "Answer", incoming.UID: "Incoming evidence"}, bodies)
	require.Empty(t, graph.Targets, "unrelated pending edges need no evidence resolution")
	require.ElementsMatch(t, []string{target.UID, reply.UID, incoming.UID}, graph.CommentUIDsByProject[project.ID])
	snapshot, err := store.(db.UIStore).ReadUISnapshot(ctx, db.UISnapshotQuery{SelectedIssueUID: source.UID})
	require.NoError(t, err)
	require.Len(t, snapshot.CommentGraph.Comments, 3)
	require.Len(t, snapshot.Comments, 1)
	require.Equal(t, reply.UID, snapshot.Comments[0].UID)

	for _, ref := range []db.CommentGraphReference{
		{UID: target.UID},
		{Suffix: strings.ToLower(target.UID[20:]), IssueID: other.ID},
		{Suffix: strings.ToLower(target.UID[20:]), IssueShortID: other.ShortID},
	} {
		identity, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, Reference: &ref})
		require.NoError(t, err)
		require.Len(t, identity.Comments, 1)
		require.Equal(t, target.UID, identity.Comments[0].Comment.UID)
		require.Equal(t, other.ID, identity.Comments[0].Comment.IssueID)
		require.Empty(t, identity.Comments[0].Comment.Body)
		require.Empty(t, identity.Targets)
		require.Empty(t, identity.CommentUIDsByProject)
	}
	query.IssueScope = &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID}
	scoped, err := store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Len(t, scoped.Comments, 1)
	require.Empty(t, scoped.Comments[0].Comment.ReplyToUID)
	require.Empty(t, scoped.Comments[0].Comment.ReplyKind)
	require.Equal(t, []string{reply.UID}, scoped.CommentUIDsByProject[project.ID])
	denied, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, IssueScope: query.IssueScope, Reference: &db.CommentGraphReference{UID: target.UID}})
	require.NoError(t, err)
	require.Empty(t, denied.Comments)
	_, _, _, err = store.SoftDeleteIssue(ctx, source.ID, "worker")
	require.NoError(t, err)
	query.IncludeDeletedSourceIssueID = source.ID
	deleted, err := store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Len(t, deleted.Comments, 1)
	require.Equal(t, reply.UID, deleted.Comments[0].Comment.UID)
	require.Empty(t, deleted.Comments[0].Comment.ReplyToUID)
	query.IncludeDeletedSourceIssueID = 0
	deleted, err = store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Empty(t, deleted.Comments)

	records, err := CollectImportRecords(ctx, store, db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true})
	require.NoError(t, err)
	countTargetCommentEvents := func() int {
		t.Helper()
		events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 1000})
		require.NoError(t, err)
		count := 0
		for _, event := range events {
			if event.Type != "issue.commented" {
				continue
			}
			var payload struct {
				CommentUID string `json:"comment_uid"`
			}
			require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
			if strings.EqualFold(payload.CommentUID, target.UID) {
				count++
			}
		}
		return count
	}
	originalTargetEvents := countTargetCommentEvents()
	require.Equal(t, 1, originalTargetEvents)
	var restoredUID string
	for index, persistedUID := range []string{strings.ToLower(target.UID), strings.ToLower(target.UID[:13]) + target.UID[13:]} {
		restoredUID = persistedUID
		for _, record := range records {
			if comment, ok := record.(*db.CommentExport); ok && comment.ID == target.ID {
				comment.UID = persistedUID
			}
		}
		require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{}))
		require.Equal(t, originalTargetEvents, countTargetCommentEvents(),
			"case-only comment UID changes do not duplicate retained creation history")
		comments, err := store.CommentsByIssue(ctx, other.ID)
		require.NoError(t, err)
		var imported *db.Comment
		for i := range comments {
			if comments[i].UID == persistedUID {
				imported = &comments[i]
				break
			}
		}
		require.NotNil(t, imported)
		require.NotNil(t, imported.AttributedTeammate)
		require.Equal(t, "writer", *imported.AttributedTeammate)
		require.Equal(t, "route", imported.Teammate)
		var exported *db.CommentExport
		for record, err := range store.ExportComments(ctx, db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}) {
			require.NoError(t, err)
			if strings.EqualFold(record.UID, persistedUID) {
				exported = &record
				break
			}
		}
		require.NotNil(t, exported)
		require.NotNil(t, exported.AttributedTeammate)
		require.Equal(t, "writer", *exported.AttributedTeammate)
		identity, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, Reference: &db.CommentGraphReference{UID: target.UID}})
		require.NoError(t, err)
		require.Len(t, identity.Comments, 1, "imported UID case must not change reference resolution")
		require.Equal(t, persistedUID, identity.Comments[0].Comment.UID)
		author := []string{"worker-one", "worker-two"}[index]
		created, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: author, Body: "Reply to imported finding", ReplyToUID: identity.Comments[0].Comment.UID, ReplyKind: "reply", ValidateReply: true})
		require.NoError(t, err)
		require.Equal(t, persistedUID, created.ReplyToUID)
		edited, _, _, err := store.EditComment(ctx, db.EditCommentParams{IssueID: other.ID, CommentUID: identity.Comments[0].Comment.UID, Actor: "finder", Body: "Updated imported finding"})
		require.NoError(t, err)
		require.Equal(t, persistedUID, edited.UID)
		require.Equal(t, "Updated imported finding", edited.Body)
	}
	_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 1000})
	require.NoError(t, err)
	var federationSnapshot *db.Event
	for i := range events {
		if events[i].Type == "issue.snapshot" && events[i].IssueUID != nil && *events[i].IssueUID == other.UID {
			federationSnapshot = &events[i]
			break
		}
	}
	require.NotNil(t, federationSnapshot)
	var snapshotComments struct {
		Comments []struct {
			CommentUID         string  `json:"comment_uid"`
			Teammate           string  `json:"teammate"`
			AttributedTeammate *string `json:"attributed_teammate"`
		} `json:"comments"`
	}
	require.NoError(t, json.Unmarshal([]byte(federationSnapshot.Payload), &snapshotComments))
	var foundSnapshotComment bool
	for _, snapshotComment := range snapshotComments.Comments {
		if !strings.EqualFold(snapshotComment.CommentUID, restoredUID) {
			continue
		}
		foundSnapshotComment = true
		require.Equal(t, "route", snapshotComment.Teammate)
		require.NotNil(t, snapshotComment.AttributedTeammate)
		require.Equal(t, "writer", *snapshotComment.AttributedTeammate)
		break
	}
	require.True(t, foundSnapshotComment)
	collider, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "finder", Body: "Collision"})
	require.NoError(t, err)
	records, err = CollectImportRecords(ctx, store, db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true})
	require.NoError(t, err)
	const findingUID = "01AAAAAAAAAAAAAAAAAAABCDEF"
	const colliderUID = "01BBBBBBBBBBBBBBBBBBABCDEF"
	for _, record := range records {
		if comment, ok := record.(*db.CommentExport); ok {
			switch comment.ID {
			case target.ID:
				comment.UID = findingUID
			case collider.ID:
				comment.UID = colliderUID
			}
		}
	}
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{}))
	destination, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: root.ID, Author: "worker", Body: "Reply to moved finding", ReplyToUID: findingUID, ReplyKind: "reply"})
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, other.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: other.ID, FromProjectID: project.ID, ToProjectID: destination.ID, IfMatchRev: current.Revision, Actor: "worker"})
	require.NoError(t, err)
	graph, err = store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: root.ID})
	require.NoError(t, err)
	require.Equal(t, "moved", graph.Targets[findingUID].Status)
	require.ElementsMatch(t, []string{findingUID, colliderUID}, graph.CommentUIDsByProject[destination.ID])
	require.Equal(t, "aabcdef", commentref.Handles(graph.CommentUIDsByProject[destination.ID])[findingUID])
	return nil
}
