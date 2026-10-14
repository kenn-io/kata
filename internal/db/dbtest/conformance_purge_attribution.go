package dbtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunPurgeAttributionAndGate checks what an issue purge leaves behind on a
// root hub, and that purge on any federated spoke needs the hub.
func RunPurgeAttributionAndGate(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: private}, "member")
	purged, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Purged task", Author: "assistant"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(writeCtx, db.CreateCommentParams{IssueID: purged.ID, Author: "assistant", Body: "Purged comment"})
	require.NoError(t, err)
	sibling, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Sibling task", Author: "assistant"})
	require.NoError(t, err)

	_, err = store.PurgeIssue(ctx, purged.ID, "admin", nil)
	require.NoError(t, err)
	_, err = store.EntityAttribution(ctx, project.UID, "issue", purged.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "purge must unlink the issue from its creation receipt")
	_, err = store.EntityAttribution(ctx, project.UID, "comment", comment.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "purge must unlink the issue's comments from their receipts")
	kept, err := store.EntityAttribution(ctx, project.UID, "issue", sibling.UID)
	require.NoError(t, err)
	assert.Equal(t, "member", kept.AccountableActor)

	// Receipt sequences are relay cursors. Purge keeps the signed receipts so
	// a later receipt cannot reuse a sequence that downstream already holds.
	receipts, err := store.AttributionReceiptsAfter(ctx, project.UID, 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, receipts, 3)
	later, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Later task", Author: "assistant"})
	require.NoError(t, err)
	laterReceipt, err := store.EntityAttribution(ctx, project.UID, "issue", later.UID)
	require.NoError(t, err)
	assert.Equal(t, int64(4), laterReceipt.Sequence)

	for _, pushEnabled := range []bool{false, true} {
		spoke, err := store.CreateProject(ctx, map[bool]string{false: "spoke-project-pull", true: "spoke-project-push"}[pushEnabled])
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.ID, Title: "Replica task", Author: "member"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: spoke.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
			HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: 1,
			Actor: "sync-agent", PushEnabled: pushEnabled, Enabled: true,
		})
		require.NoError(t, err)
		_, err = store.PurgeIssue(ctx, issue.ID, "admin", nil)
		require.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported, "push enabled: %v", pushEnabled)
	}
}
