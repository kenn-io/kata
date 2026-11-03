package dbtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
)

func checkCommentDisplayContribution(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "display-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Display", Author: "actor"})
	require.NoError(t, err)
	comment, event, err := store.CreateComment(teammate.WithRequest(ctx, "writer"), db.CreateCommentParams{IssueID: issue.ID, Author: "actor", Teammate: "route", Body: "Original", IdempotencyKey: "display-key", IdempotencyFingerprint: "fingerprint"})
	require.NoError(t, err)
	orderProject, err := store.CreateProject(ctx, "comment-order-project")
	require.NoError(t, err)
	firstIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: orderProject.ID, Title: "First", Author: "actor"})
	require.NoError(t, err)
	interleavedIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: orderProject.ID, Title: "Interleaved", Author: "actor"})
	require.NoError(t, err)
	firstIssueComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: firstIssue.ID, Author: "actor", Body: "First issue comment"})
	require.NoError(t, err)
	secondIssueComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: interleavedIssue.ID, Author: "actor", Body: "Interleaved comment"})
	require.NoError(t, err)
	lastIssueComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: firstIssue.ID, Author: "actor", Body: "Last issue comment"})
	require.NoError(t, err)
	var exportedCommentIDs []int64
	for exported, err := range store.ExportComments(ctx, db.ExportFilter{ProjectID: &orderProject.ID}) {
		require.NoError(t, err)
		exportedCommentIDs = append(exportedCommentIDs, exported.ID)
	}
	require.Equal(t, []int64{firstIssueComment.ID, secondIssueComment.ID, lastIssueComment.ID}, exportedCommentIDs,
		"comment exports preserve global ID order when issues have interleaved comments")
	assertDisplay := func(value any) {
		t.Helper()
		data, err := json.Marshal(value)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(data, &fields))
		require.Equal(t, "writer", fields["attributed_teammate"])
		require.Equal(t, "route", fields["teammate"])
	}
	assertDisplay(comment)
	edited, _, changed, err := store.EditComment(teammate.WithRequest(ctx, "editor"), db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "other", Body: "Edited"})
	require.NoError(t, err)
	require.True(t, changed)
	assertDisplay(edited)
	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assertDisplay(comments[0])
	retry, err := store.LookupCommentIdempotency(ctx, issue.UID, "display-key", time.Time{})
	require.NoError(t, err)
	require.NotNil(t, retry)
	assertDisplay(retry.Comment)
	require.Equal(t, event.ContentHash, retry.Event.ContentHash)
	graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: issue.ID})
	require.NoError(t, err)
	require.Len(t, graph.Comments, 1)
	assertDisplay(graph.Comments[0].Comment)
	snapshot, err := store.(db.UIStore).ReadUISnapshot(ctx, db.UISnapshotQuery{SelectedIssueUID: issue.UID})
	require.NoError(t, err)
	require.Len(t, snapshot.Comments, 1)
	assertDisplay(snapshot.Comments[0])

	// The separate creation receipt continues to name the routing identity.
	signedProject, err := store.CreateProject(ctx, "receipt-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: signedProject.ID, Role: db.FederationRoleHub, HubProjectID: signedProject.ID, HubProjectUID: signedProject.UID, Enabled: true})
	require.NoError(t, err)
	signedIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: signedProject.ID, Title: "Signed creation", Author: "actor"})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: signedProject.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	signedCtx := db.WithRootAttribution(teammate.WithRequest(ctx, "writer"), db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: private}, "member")
	signed, _, err := store.CreateComment(signedCtx, db.CreateCommentParams{IssueID: signedIssue.ID, Author: "actor", Teammate: "route", Body: "Signed creation"})
	require.NoError(t, err)
	receipt, err := store.EntityAttribution(ctx, signedProject.UID, "comment", signed.UID)
	require.NoError(t, err)
	assertDisplay(signed)
	require.Equal(t, "route", receipt.Teammate)
	_, _, _, err = store.EditComment(teammate.WithRequest(ctx, "editor"), db.EditCommentParams{IssueID: signedIssue.ID, CommentUID: signed.UID, Actor: "other", Body: "Revised signed creation"})
	require.NoError(t, err)
	receiptAfter, err := store.EntityAttribution(ctx, signedProject.UID, "comment", signed.UID)
	require.NoError(t, err)
	require.Equal(t, receipt, receiptAfter)

	// A reply from the old project still displays a target moved to another one.
	replyIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Reply", Author: "actor"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: replyIssue.ID, Author: "reviewer", Body: "Response", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	fallbackIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Fallback", Author: "actor"})
	require.NoError(t, err)
	fallbackComment, _, err := store.CreateComment(teammate.WithRequest(ctx, "route"), db.CreateCommentParams{
		IssueID: fallbackIssue.ID, Author: "actor", Teammate: "route", Body: "Legacy display fallback",
	})
	require.NoError(t, err)
	movedProject, err := store.CreateProject(ctx, "moved-project")
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: project.ID, ToProjectID: movedProject.ID, IfMatchRev: current.Revision, Actor: "actor"})
	require.NoError(t, err)
	fallbackCurrent, err := store.IssueByID(ctx, fallbackIssue.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: fallbackIssue.ID, FromProjectID: project.ID, ToProjectID: movedProject.ID, IfMatchRev: fallbackCurrent.Revision, Actor: "actor"})
	require.NoError(t, err)
	graph, err = store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: replyIssue.ID})
	require.NoError(t, err)
	require.Equal(t, "moved", graph.Targets[comment.UID].Status)
	require.NotNil(t, graph.Targets[comment.UID].Record)
	assertDisplay(graph.Targets[comment.UID].Record.Comment)

	// A project-only export must retain comment display data when the original
	// creation event remains in the source project after the issue moves.
	destinationRecords, err := CollectImportRecords(ctx, store, db.ExportFilter{ProjectID: &movedProject.ID, IncludeDeleted: true})
	require.NoError(t, err)
	var exportedComment *db.CommentExport
	for _, record := range destinationRecords {
		if commentRecord, ok := record.(*db.CommentExport); ok && commentRecord.UID == comment.UID {
			exportedComment = commentRecord
			break
		}
	}
	require.NotNil(t, exportedComment)
	require.NotNil(t, exportedComment.AttributedTeammate)
	require.Equal(t, "writer", *exportedComment.AttributedTeammate)
	exportedCommentJSON, err := json.Marshal(exportedComment)
	require.NoError(t, err)
	var exportedCommentFields map[string]any
	require.NoError(t, json.Unmarshal(exportedCommentJSON, &exportedCommentFields))
	require.Equal(t, "writer", exportedCommentFields["attributed_teammate"])
	destinationRestore := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, destinationRestore.Close()) })
	require.NoError(t, destinationRestore.ImportReplay(ctx, destinationRecords, db.ImportOptions{}))
	destinationIssue, err := destinationRestore.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	destinationComments, err := destinationRestore.CommentsByIssue(ctx, destinationIssue.ID)
	require.NoError(t, err)
	require.Len(t, destinationComments, 1)
	assertDisplay(destinationComments[0])
	require.NotNil(t, edited.EditedAt)
	destinationEvents, err := destinationRestore.EventsAfter(ctx, db.EventsAfterParams{ProjectID: destinationIssue.ProjectID, Limit: 1000})
	require.NoError(t, err)
	synthesizedEditedAt := false
	for _, destinationEvent := range destinationEvents {
		if destinationEvent.Type != "issue.commented" {
			continue
		}
		var payload struct {
			CommentUID string `json:"comment_uid"`
			EditedAt   string `json:"edited_at"`
		}
		require.NoError(t, json.Unmarshal([]byte(destinationEvent.Payload), &payload))
		if payload.CommentUID != comment.UID {
			continue
		}
		require.NotEmpty(t, payload.EditedAt,
			"destination-only replay must retain an edited comment's timestamp in its synthesized creation event")
		parsedEditedAt, err := time.Parse(time.RFC3339Nano, payload.EditedAt)
		require.NoError(t, err)
		require.True(t, edited.EditedAt.Equal(parsedEditedAt),
			"destination-only replay must retain an edited comment's timestamp in its synthesized creation event")
		synthesizedEditedAt = true
	}
	require.True(t, synthesizedEditedAt, "destination-only replay should synthesize the moved comment's display event")
	destinationFallbackIssue, err := destinationRestore.IssueByUID(ctx, fallbackIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	destinationFallbackComments, err := destinationRestore.CommentsByIssue(ctx, destinationFallbackIssue.ID)
	require.NoError(t, err)
	require.Len(t, destinationFallbackComments, 1)
	require.NotNil(t, destinationFallbackComments[0].AttributedTeammate)
	require.Equal(t, "route", *destinationFallbackComments[0].AttributedTeammate)
	fallbackEvents, err := destinationRestore.EventsAfter(ctx, db.EventsAfterParams{ProjectID: destinationFallbackIssue.ProjectID, Limit: 1000})
	require.NoError(t, err)
	for _, fallbackEvent := range fallbackEvents {
		if fallbackEvent.Type != "issue.commented" {
			continue
		}
		var payload struct {
			CommentUID string `json:"comment_uid"`
		}
		require.NoError(t, json.Unmarshal([]byte(fallbackEvent.Payload), &payload))
		require.NotEqual(t, fallbackComment.UID, payload.CommentUID,
			"restoring the routing fallback must not synthesize a new visible comment event")
	}

	// Existing portable exports carry display in retained events, not new row fields.
	records, err := CollectImportRecords(ctx, store, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{}))
	restoredIssue, err := restored.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	restoredComments, err := restored.CommentsByIssue(ctx, restoredIssue.ID)
	require.NoError(t, err)
	require.Len(t, restoredComments, 1)
	for _, c := range restoredComments {
		assertDisplay(c)
	}
	restoredEvents, err := restored.EventsByUIDs(ctx, project.ID, []string{event.UID})
	require.NoError(t, err)
	require.Len(t, restoredEvents, 1)
	require.Equal(t, event.Payload, restoredEvents[0].Payload)
	require.Equal(t, event.ContentHash, restoredEvents[0].ContentHash)

	// Both federation snapshot builders retain the creator independently of edits.
	_, err = store.EnableProjectFederation(ctx, movedProject.ID, "actor")
	require.NoError(t, err)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: movedProject.ID, Limit: 100})
	require.NoError(t, err)
	var retained *db.Event
	retainedByIssueUID := make(map[string]db.Event)
	for _, e := range events {
		if e.Type == "issue.snapshot" && e.IssueUID != nil && *e.IssueUID == issue.UID {
			retained = &e
		}
		if e.Type == "issue.snapshot" && e.IssueUID != nil {
			retainedByIssueUID[*e.IssueUID] = e
		}
	}
	require.NotNil(t, retained)
	var document struct {
		Comments []map[string]any `json:"comments"`
	}
	require.NoError(t, json.Unmarshal([]byte(retained.Payload), &document))
	require.Len(t, document.Comments, 1)
	for _, c := range document.Comments {
		require.Equal(t, "writer", c["attributed_teammate"])
		require.Equal(t, "route", c["teammate"])
	}
	destinationProject, err := destinationRestore.ProjectByUID(ctx, movedProject.UID)
	require.NoError(t, err)
	_, err = destinationRestore.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: destinationProject.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: destinationProject.UID, Enabled: true})
	require.NoError(t, err)
	for index, issueUID := range []string{issue.UID, fallbackIssue.UID} {
		snapshot, ok := retainedByIssueUID[issueUID]
		require.True(t, ok, "federation setup should snapshot each moved issue")
		uid := issueUID
		remoteSnapshot := newRemoteEvent(t, destinationProject, &uid, "issue.snapshot", "actor", "01EEEEEEEEEEEEEEEEEEEEEEEE", time.Now().Add(time.Duration(index+1)*time.Hour).UnixMilli(), jsontext.Value(snapshot.Payload))
		_, err = destinationRestore.InsertRemoteEvent(ctx, destinationProject.ID, remoteSnapshot)
		require.NoError(t, err)
	}
	require.NoError(t, destinationRestore.MaterializeFederatedProject(ctx, destinationProject.ID))
	destinationIssue, err = destinationRestore.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	destinationComments, err = destinationRestore.CommentsByIssue(ctx, destinationIssue.ID)
	require.NoError(t, err)
	require.Len(t, destinationComments, 1)
	require.NotNil(t, destinationComments[0].EditedAt)
	require.True(t, edited.EditedAt.Equal(*destinationComments[0].EditedAt),
		"federation materialization must retain the edited timestamp after destination-only restore")

	// Restore from the snapshot alone, without original creation events or rows.
	snapshotStore := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, snapshotStore.Close()) })
	p, err := snapshotStore.CreateProject(ctx, "snapshot-project")
	require.NoError(t, err)
	_, err = snapshotStore.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: p.UID, Enabled: true})
	require.NoError(t, err)
	snapshotEvent := newRemoteEvent(t, p, &issue.UID, "issue.snapshot", "actor", "01DDDDDDDDDDDDDDDDDDDDDDDD", 1, jsontext.Value(retained.Payload))
	_, err = snapshotStore.InsertRemoteEvent(ctx, p.ID, snapshotEvent)
	require.NoError(t, err)
	require.NoError(t, snapshotStore.MaterializeFederatedProject(ctx, p.ID))
	snapshotIssue, err := snapshotStore.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	snapshotComments, err := snapshotStore.CommentsByIssue(ctx, snapshotIssue.ID)
	require.NoError(t, err)
	require.Len(t, snapshotComments, 1)
	for _, c := range snapshotComments {
		assertDisplay(c)
	}
	// A malformed display field omits only that label, retaining the whole snapshot.
	malformed := make(map[string]jsontext.Value)
	require.NoError(t, json.Unmarshal([]byte(retained.Payload), &malformed))
	document.Comments[0]["attributed_teammate"] = 42
	malformed["comments"], err = json.Marshal(document.Comments)
	require.NoError(t, err)
	payload, err := json.Marshal(malformed)
	require.NoError(t, err)
	malformedStore := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, malformedStore.Close()) })
	invalidProject, err := malformedStore.CreateProject(ctx, "invalid-display-project")
	require.NoError(t, err)
	_, err = malformedStore.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: invalidProject.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: invalidProject.UID, Enabled: true})
	require.NoError(t, err)
	invalidDisplay := newRemoteEvent(t, invalidProject, &issue.UID, "issue.snapshot", "actor", snapshotEvent.OriginInstanceUID, 1, payload)
	_, err = malformedStore.InsertRemoteEvent(ctx, invalidProject.ID, invalidDisplay)
	require.NoError(t, err)
	require.NoError(t, malformedStore.MaterializeFederatedProject(ctx, invalidProject.ID))
	invalidIssue, err := malformedStore.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	snapshotComments, err = malformedStore.CommentsByIssue(ctx, invalidIssue.ID)
	require.NoError(t, err)
	require.Len(t, snapshotComments, 1)
	for _, c := range snapshotComments {
		require.NotNil(t, c.AttributedTeammate)
		if c.UID == document.Comments[0]["comment_uid"] {
			require.Empty(t, *c.AttributedTeammate)
		} else {
			assertDisplay(c)
		}
		require.Equal(t, "route", c.Teammate)
		require.NotEmpty(t, c.Body)
	}

	t.Run("federation adoption", func(t *testing.T) {
		checkAdoptionCommentDisplay(t, backend)
	})
	return nil
}

func checkEmptyCommentDisplayContribution(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "suppressed-display-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Suppressed display", Author: "actor"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(teammate.WithRequest(ctx, ""), db.CreateCommentParams{
		IssueID: issue.ID, Author: "actor", Teammate: "route", Body: "Explicitly suppressed",
	})
	require.NoError(t, err)
	require.NotNil(t, comment.AttributedTeammate)
	require.Empty(t, *comment.AttributedTeammate)
	assertSuppressed := func(value any) {
		t.Helper()
		data, err := json.Marshal(value)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(data, &fields))
		require.Contains(t, fields, "attributed_teammate")
		require.Equal(t, "", fields["attributed_teammate"])
		require.Equal(t, "route", fields["teammate"])
	}
	assertSuppressed(comment)
	var exportedSuppressed *db.CommentExport
	for record, err := range store.ExportComments(ctx, db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}) {
		require.NoError(t, err)
		if record.UID == comment.UID {
			exportedSuppressed = &record
			break
		}
	}
	require.NotNil(t, exportedSuppressed)
	require.NotNil(t, exportedSuppressed.AttributedTeammate)
	require.Empty(t, *exportedSuppressed.AttributedTeammate)
	exportedSuppressedJSON, err := json.Marshal(exportedSuppressed)
	require.NoError(t, err)
	var exportedSuppressedFields map[string]any
	require.NoError(t, json.Unmarshal(exportedSuppressedJSON, &exportedSuppressedFields))
	require.Contains(t, exportedSuppressedFields, "attributed_teammate")
	require.Equal(t, "", exportedSuppressedFields["attributed_teammate"])

	_, err = store.EnableProjectFederation(ctx, project.ID, "actor")
	require.NoError(t, err)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	var snapshot *db.Event
	for i := range events {
		if events[i].Type == "issue.snapshot" && events[i].IssueUID != nil && *events[i].IssueUID == issue.UID {
			snapshot = &events[i]
			break
		}
	}
	require.NotNil(t, snapshot)
	var document struct {
		Comments []map[string]any `json:"comments"`
	}
	require.NoError(t, json.Unmarshal([]byte(snapshot.Payload), &document))
	require.Len(t, document.Comments, 1)
	require.Contains(t, document.Comments[0], "attributed_teammate")
	require.Equal(t, "", document.Comments[0]["attributed_teammate"])
	require.Equal(t, "route", document.Comments[0]["teammate"])

	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	target, err := restored.CreateProject(ctx, "suppressed-snapshot-project")
	require.NoError(t, err)
	_, err = restored.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: target.UID, Enabled: true,
	})
	require.NoError(t, err)
	remote := newRemoteEvent(t, target, &issue.UID, "issue.snapshot", "actor", snapshot.OriginInstanceUID, 1, jsontext.Value(snapshot.Payload))
	_, err = restored.InsertRemoteEvent(ctx, target.ID, remote)
	require.NoError(t, err)
	require.NoError(t, restored.MaterializeFederatedProject(ctx, target.ID))
	restoredIssue, err := restored.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	restoredComments, err := restored.CommentsByIssue(ctx, restoredIssue.ID)
	require.NoError(t, err)
	require.Len(t, restoredComments, 1)
	require.NotNil(t, restoredComments[0].AttributedTeammate)
	require.Empty(t, *restoredComments[0].AttributedTeammate)
	assertSuppressed(restoredComments[0])
	return nil
}

// Adoption must read creation history before replacing it with snapshots.
func checkAdoptionCommentDisplay(t *testing.T, backend Backend) {
	ctx := t.Context()
	store := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "adoption-display-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Adopt display", Author: "actor"})
	require.NoError(t, err)
	wantRouting := map[string]string{}
	for _, route := range []string{"", "route"} {
		comment, _, err := store.CreateComment(teammate.WithRequest(ctx, "writer"), db.CreateCommentParams{
			IssueID: issue.ID, Author: "actor", Teammate: route, Body: "Original",
		})
		require.NoError(t, err)
		wantRouting[comment.UID] = route
		_, _, changed, err := store.EditComment(teammate.WithRequest(ctx, "editor"), db.EditCommentParams{
			IssueID: issue.ID, CommentUID: comment.UID, Actor: "other", Body: "Edited",
		})
		require.NoError(t, err)
		require.True(t, changed)
	}
	assertComments := func(comments []db.Comment) {
		t.Helper()
		require.Len(t, comments, 2)
		for _, comment := range comments {
			route, ok := wantRouting[comment.UID]
			require.True(t, ok)
			assert.Equal(t, "actor", comment.Author)
			assert.Equal(t, route, comment.Teammate)
			assert.Equal(t, "Edited", comment.Body)
			if assert.NotNil(t, comment.AttributedTeammate) {
				assert.Equal(t, "writer", *comment.AttributedTeammate, "comment %s routing %q", comment.UID, route)
			}
		}
	}
	before, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	assertComments(before)
	adopted, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", Actor: "operator",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), adopted.AdoptionSnapshotCount)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "issue.snapshot", events[0].Type)
	var document struct {
		Comments []struct {
			UID                string  `json:"comment_uid"`
			Author             string  `json:"author"`
			Teammate           string  `json:"teammate"`
			AttributedTeammate *string `json:"attributed_teammate"`
			Body               string  `json:"body"`
		} `json:"comments"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &document))
	var snapshotComments []db.Comment
	for _, comment := range document.Comments {
		snapshotComments = append(snapshotComments, db.Comment{
			UID: comment.UID, Author: comment.Author, Teammate: comment.Teammate,
			AttributedTeammate: comment.AttributedTeammate, Body: comment.Body,
		})
	}
	assertComments(snapshotComments)
	after, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	assertComments(after)

	// Replay only the adoption snapshot, without any creation or edit events.
	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	target, err := restored.CreateProject(ctx, "adoption-snapshot-project")
	require.NoError(t, err)
	_, err = restored.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: target.UID, Enabled: true})
	require.NoError(t, err)
	remote := newRemoteEvent(t, target, &issue.UID, "issue.snapshot", "operator", "01DDDDDDDDDDDDDDDDDDDDDDDD", 1, jsontext.Value(events[0].Payload))
	_, err = restored.InsertRemoteEvent(ctx, target.ID, remote)
	require.NoError(t, err)
	require.NoError(t, restored.MaterializeFederatedProject(ctx, target.ID))
	restoredIssue, err := restored.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := restored.CommentsByIssue(ctx, restoredIssue.ID)
	require.NoError(t, err)
	assertComments(comments)
}
