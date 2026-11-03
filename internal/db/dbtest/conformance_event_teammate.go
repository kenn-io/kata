package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
)

// The contribution is request display data: native writes hash it, while
// system events and replay retain their own bytes and identities.
func checkEventTeammate(t *testing.T, store db.Storage, backend Backend) error {
	ctx := teammate.WithRequest(t.Context(), "worker-1")
	project, err := store.CreateProject(ctx, "contribution-project")
	require.NoError(t, err)
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Work", Author: "coordinator", Metadata: map[string]jsontext.Value{"teammate": []byte(`"explicit-owner"`)}})
	require.NoError(t, err)
	check := func(event db.Event) {
		t.Helper()
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
		require.Equal(t, "worker-1", payload["attributed_teammate"], event.Type)
		require.NotEmpty(t, event.ContentHash)
		hash, hashErr := db.EventContentHash(db.EventHashInput{UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: []byte(event.Payload)})
		require.NoError(t, hashErr)
		require.Equal(t, event.ContentHash, hash, event.Type)
	}
	check(created)
	_, _, err = store.CreateIssue(teammate.WithRequest(ctx, "bad/handle"), db.CreateIssueParams{ProjectID: project.ID, Title: "Invalid contribution", Author: "coordinator"})
	require.ErrorContains(t, err, "teammate")
	peer, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Peer work", Author: "coordinator"})
	require.NoError(t, err)
	atomic, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "coordinator", Title: new("Updated work"), AddRelated: []int64{peer.ID}})
	require.NoError(t, err)
	require.Len(t, atomic.Events, 2)
	for _, event := range atomic.Events {
		check(event)
	}
	linkParams := db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "coordinator"}
	link, linked, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "coordinator"}, linkParams)
	require.NoError(t, err)
	check(linked)
	linkParams.EventType = "issue.unlinked"
	unlinked, err := store.DeleteLinkAndEvent(ctx, link, linkParams)
	require.NoError(t, err)
	check(unlinked)
	require.Contains(t, created.Payload, `"teammate":"explicit-owner"`)
	comment, event, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "coordinator", Body: "progress", Teammate: "explicit-comment"})
	require.NoError(t, err)
	check(event)
	require.Equal(t, "explicit-comment", comment.Teammate)
	_, edited, _, err := store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "coordinator", Body: "revised"})
	require.NoError(t, err)
	require.NotNil(t, edited)
	check(*edited)
	_, owned, _, err := store.UpdateOwner(ctx, issue.ID, new("coordinator"), "coordinator")
	require.NoError(t, err)
	check(*owned)
	_, priority, _, err := store.UpdatePriority(ctx, issue.ID, new(int64(2)), "coordinator")
	require.NoError(t, err)
	check(*priority)
	_, cleared, _, err := store.UpdatePriority(ctx, issue.ID, nil, "coordinator")
	require.NoError(t, err)
	check(*cleared)
	_, unowned, _, err := store.UpdateOwner(ctx, issue.ID, nil, "coordinator")
	require.NoError(t, err)
	check(*unowned)
	now := time.Now().UTC()
	assigned, err := store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: "coordinator", TTL: time.Hour, Now: now})
	require.NoError(t, err)
	check(*assigned.Event)
	renewed, err := store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: "coordinator", TTL: time.Hour, Now: now.Add(time.Minute)})
	require.NoError(t, err)
	require.Equal(t, "issue.assignment_renewed", renewed.Event.Type)
	check(*renewed.Event)
	_, label, err := store.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "ready", Actor: "coordinator"})
	require.NoError(t, err)
	check(label)
	unlabel, err := store.RemoveLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.unlabeled", Label: "ready", Actor: "coordinator"})
	require.NoError(t, err)
	check(unlabel)
	patch, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "coordinator", Patch: map[string]jsontext.Value{"note": []byte(`"progress"`)}})
	require.NoError(t, err)
	check(patch.Event)
	_, closed, _, err := store.CloseIssue(ctx, issue.ID, "done", "coordinator", "Verified requested work.", []db.Evidence{{Type: "test", Command: "go test ./internal/example"}})
	require.NoError(t, err)
	check(*closed)
	_, reopened, _, err := store.ReopenIssue(ctx, issue.ID, "coordinator")
	require.NoError(t, err)
	check(*reopened)
	_, deleted, _, err := store.SoftDeleteIssue(ctx, issue.ID, "coordinator")
	require.NoError(t, err)
	check(*deleted)
	_, restored, _, err := store.RestoreIssue(ctx, issue.ID, "coordinator")
	require.NoError(t, err)
	check(*restored)
	principal := db.ClaimPrincipal{Holder: "coordinator", HolderInstanceUID: created.OriginInstanceUID, ClientKind: "agent"}
	lease, err := store.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: project.ID, IssueRef: issue.UID, Principal: principal, ClaimKind: "timed", TTL: time.Hour})
	require.NoError(t, err)
	check(*lease.Event)
	released, err := store.ReleaseClaim(ctx, db.ReleaseClaimParams{ProjectID: project.ID, IssueRef: issue.UID, Principal: principal})
	require.NoError(t, err)
	check(*released.Event)
	lease, err = store.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: project.ID, IssueRef: issue.UID, Principal: principal, ClaimKind: "hard"})
	require.NoError(t, err)
	check(*lease.Event)
	forced, err := store.ForceReleaseClaim(ctx, db.ForceReleaseClaimParams{ProjectID: project.ID, IssueRef: issue.UID, Actor: "coordinator", Reason: "operator release"})
	require.NoError(t, err)
	check(*forced.Event)
	// Work performed by another actor at a hub produces violation/release events too.
	hubProject, err := store.CreateProject(t.Context(), "claim-hub")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(t.Context(), hubProject.ID, "coordinator")
	require.NoError(t, err)
	hubIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: hubProject.ID, Title: "Leased work", Author: "coordinator"})
	require.NoError(t, err)
	lease, err = store.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: hubProject.ID, IssueRef: hubIssue.UID, Principal: principal, ClaimKind: "hard"})
	require.NoError(t, err)
	_, closeEvents, _, err := store.CloseIssueWithEvents(ctx, hubIssue.ID, "done", "other-actor", "Verified requested work.", []db.Evidence{{Type: "test", Command: "go test ./internal/example"}})
	require.NoError(t, err)
	var violation bool
	for _, event := range closeEvents {
		check(event)
		violation = violation || event.Type == "claim.violated"
	}
	require.True(t, violation)
	targetProject, err := store.CreateProject(t.Context(), "destination-project")
	require.NoError(t, err)
	latest, err := store.IssueByID(ctx, peer.ID)
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: project.ID, ToProjectID: targetProject.ID, IfMatchRev: latest.Revision, Actor: "coordinator"})
	require.NoError(t, err)
	check(*moved.Event)
	// A native system write cannot inherit the user contribution.
	_, system, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "System work", Author: "system"})
	require.NoError(t, err)
	require.NotContains(t, system.Payload, "attributed_teammate")
	_, empty, err := store.CreateIssue(teammate.WithRequest(ctx, ""), db.CreateIssueParams{ProjectID: project.ID, Title: "Unattributed work", Author: "coordinator"})
	require.NoError(t, err)
	require.NotContains(t, empty.Payload, "attributed_teammate")
	importedAt := time.Now().UTC()
	_, importedEvents, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID,
		Source:    "external-fixture",
		Actor:     "importer",
		Items: []db.ImportItem{{
			ExternalID: "external-issue",
			Title:      "Imported work",
			Author:     "external-author",
			Status:     "open",
			CreatedAt:  importedAt,
			UpdatedAt:  importedAt,
			Comments: []db.ImportComment{{
				ExternalID: "external-comment",
				Author:     "external-author",
				Body:       "Imported history",
				CreatedAt:  importedAt,
			}},
		}},
	})
	require.NoError(t, err)
	require.Len(t, importedEvents, 2)
	for _, imported := range importedEvents {
		require.NotContains(t, imported.Payload, "attributed_teammate", imported.Type)
	}
	_, nativeAfterImport, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Native after import", Author: "coordinator"})
	require.NoError(t, err)
	check(nativeAfterImport)
	records, err := CollectImportRecords(t.Context(), store, db.ExportFilter{})
	require.NoError(t, err)
	target := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportReplay(teammate.WithRequest(t.Context(), "other-worker"), records, db.ImportOptions{}))
	importedProject, err := target.ProjectByUID(t.Context(), project.UID)
	require.NoError(t, err)
	retained, err := target.EventsByUIDs(t.Context(), importedProject.ID, []string{created.UID, event.UID, empty.UID})
	require.NoError(t, err)
	require.Len(t, retained, 3)
	originals := map[string]db.Event{created.UID: created, event.UID: event, empty.UID: empty}
	for _, got := range retained {
		require.Equal(t, originals[got.UID].Payload, got.Payload)
		require.Equal(t, originals[got.UID].ContentHash, got.ContentHash)
	}
	return nil
}
