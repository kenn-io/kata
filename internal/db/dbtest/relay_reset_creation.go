package dbtest

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunCreateRelayResetAfterCompaction exercises create relay reset after compaction on the supplied native store.
// R4/R5: compacted roots supply signed current state and original creation
// proofs. Lost responses retry one durable checkpoint, not new snapshot IDs.
func RunCreateRelayResetAfterCompaction(t *testing.T, store db.Storage, compact func(context.Context, int64) error) {
	creator, ok := store.(interface {
		CreateRelayReset(context.Context, string, db.RootAttributionSigner) (db.RelayResetCheckpoint, error)
	})
	require.True(t, ok, "native root checkpoint generation is required")
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "reset-root-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
	legacy, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Historical legacy issue", Author: "legacy-author"})
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, signer, "company-member")
	issue, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Original title", Author: "source-assistant"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(writeCtx, db.CreateCommentParams{IssueID: issue.ID, Author: "comment-assistant", Teammate: "reviewer", Body: "Original comment"})
	require.NoError(t, err)
	title := "Current title"
	_, _, _, err = store.EditIssue(writeCtx, db.EditIssueParams{IssueID: issue.ID, Actor: "editor", Title: &title})
	require.NoError(t, err)
	_, _, _, err = store.EditComment(writeCtx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "editor", Body: "Current comment"})
	require.NoError(t, err)
	issueProof, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	commentProof, err := store.EntityAttribution(ctx, project.UID, "comment", comment.UID)
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "checkpoint-parent-test-token", Actor: "company-member", AdminActor: "admin"})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "checkpoint-child-test-token"})
	require.NoError(t, err)
	bindingUID := grant.Enrollment.RelayBindingUID
	// Delivery already accepted by this child may be compacted. A reset must not
	// silently discard any outstanding delivery under its old epoch.
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt} {
		for {
			batch, err := store.PendingRelayDeliveries(ctx, bindingUID, stream, 100)
			require.NoError(t, err)
			if len(batch) == 0 {
				break
			}
			last := batch[len(batch)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, bindingUID, 1, stream, last.Sequence, last.Digest))
		}
	}
	require.NoError(t, compact(ctx, project.ID))
	checkpoint, err := creator.CreateRelayReset(ctx, bindingUID, signer)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	require.NoError(t, db.ValidateRelayResetTranslation(checkpoint.Translation.Authority, checkpoint.Manifest, checkpoint.Translation))
	require.Equal(t, store.InstanceUID(), checkpoint.Translation.Authority.SenderInstanceUID)
	require.Equal(t, grant.Enrollment.SpokeInstanceUID, checkpoint.Translation.Authority.ReceiverInstanceUID)
	require.Greater(t, checkpoint.Translation.Authority.Epoch, int64(1))
	decoded, provenance, err := db.DecodeRootResetPayload(pin, checkpoint.Snapshot)
	require.NoError(t, err)
	folds := []db.FoldEvent{}
	for _, event := range decoded {
		fold := db.FoldEvent{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: event.Payload}
		if event.IssueUID != nil {
			fold.IssueUID = *event.IssueUID
		}
		folds = append(folds, fold)
	}
	projection := db.FoldEvents(folds)
	require.Equal(t, "Current title", projection.Issues[issue.UID].Title)
	require.Equal(t, "Current comment", projection.Comments[comment.UID].Body)
	require.Equal(t, "comment-assistant", projection.Comments[comment.UID].Author)
	require.Equal(t, "reviewer", projection.Comments[comment.UID].Teammate)
	require.Contains(t, provenance.Receipts, issueProof)
	require.Contains(t, provenance.Receipts, commentProof)
	require.Contains(t, provenance.Entities, db.EntityProvenance{ProjectUID: project.UID, Kind: "issue", EntityUID: issue.UID, EventUID: issueProof.EventUID})
	require.Contains(t, provenance.Entities, db.EntityProvenance{ProjectUID: project.UID, Kind: "comment", EntityUID: comment.UID, EventUID: commentProof.EventUID})
	for _, ref := range provenance.Entities {
		require.NotEqual(t, legacy.UID, ref.EntityUID, "snapshot signing never manufactures legacy creation attribution")
	}
	retry, err := creator.CreateRelayReset(ctx, bindingUID, signer)
	require.NoError(t, err)
	require.Equal(t, checkpoint, retry)
	_, wrongPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	_, err = creator.CreateRelayReset(ctx, bindingUID, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: wrongPrivate})
	require.Error(t, err, "retry cannot bypass owner signing-key checks")
	newPublic, newPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	nextPin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(newPublic), PublicKey: newPublic}
	transition, err := db.SignRootKeyTransition(pin, nextPin, private)
	require.NoError(t, err)
	require.NoError(t, store.RotateRootAuthority(ctx, transition))
	rotatedRetry, err := creator.CreateRelayReset(ctx, bindingUID, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: newPrivate})
	require.NoError(t, err, "known historical root signatures survive rotation without re-signing the checkpoint")
	require.Equal(t, checkpoint, rotatedRetry)
	_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
	require.NoError(t, err)
	_, err = creator.CreateRelayReset(ctx, bindingUID, signer)
	require.Error(t, err, "revocation applies before cached checkpoint disclosure")
}
