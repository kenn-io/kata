package dbtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentDisplayContribution(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "display-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Display", Author: "actor"})
	require.NoError(t, err)
	comment, event, err := store.CreateComment(db.WithEventTeammate(ctx, "writer"), db.CreateCommentParams{IssueID: issue.ID, Author: "actor", Teammate: "route", Body: "Original", IdempotencyKey: "display-key", IdempotencyFingerprint: "fingerprint"})
	require.NoError(t, err)
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
	edited, _, changed, err := store.EditComment(db.WithEventTeammate(ctx, "editor"), db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "other", Body: "Edited"})
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
	signedCtx := db.WithRootAttribution(db.WithEventTeammate(ctx, "writer"), db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: private}, "member")
	signed, _, err := store.CreateComment(signedCtx, db.CreateCommentParams{IssueID: signedIssue.ID, Author: "actor", Teammate: "route", Body: "Signed creation"})
	require.NoError(t, err)
	receipt, err := store.EntityAttribution(ctx, signedProject.UID, "comment", signed.UID)
	require.NoError(t, err)
	assertDisplay(signed)
	require.Equal(t, "route", receipt.Teammate)
	_, _, _, err = store.EditComment(db.WithEventTeammate(ctx, "editor"), db.EditCommentParams{IssueID: signedIssue.ID, CommentUID: signed.UID, Actor: "other", Body: "Revised signed creation"})
	require.NoError(t, err)
	receiptAfter, err := store.EntityAttribution(ctx, signedProject.UID, "comment", signed.UID)
	require.NoError(t, err)
	require.Equal(t, receipt, receiptAfter)

	// A reply from the old project still displays a target moved to another one.
	replyIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Reply", Author: "actor"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: replyIssue.ID, Author: "reviewer", Body: "Response", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	movedProject, err := store.CreateProject(ctx, "moved-project")
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: project.ID, ToProjectID: movedProject.ID, IfMatchRev: current.Revision, Actor: "actor"})
	require.NoError(t, err)
	graph, err = store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: replyIssue.ID})
	require.NoError(t, err)
	require.Equal(t, "moved", graph.Targets[comment.UID].Status)
	require.NotNil(t, graph.Targets[comment.UID].Record)
	assertDisplay(graph.Targets[comment.UID].Record.Comment)

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
	for _, e := range events {
		if e.Type == "issue.snapshot" && e.IssueUID != nil && *e.IssueUID == issue.UID {
			retained = &e
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
	var malformed map[string]jsontext.Value
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
	return nil
}
