package dbtest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A spoke stops pushing only after the hub accepted its local changes and the
// replica holds no claim work. Refusal changes nothing; success keeps cursors,
// tasks, and events so a later push enablement continues from the same place.
func checkFederationPushDisableWaitsForLocalWork(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "local work", Author: "bound-agent"})
	require.NoError(t, err)
	hubUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: hubUID,
		ReplayHorizonEventID: 9, PullCursorEventID: 8, PushEnabled: true,
		Actor: "bound-agent", Enabled: true,
	})
	require.NoError(t, err)

	_, err = store.DisableFederationPushWhenSettled(ctx, project.ID, store.InstanceUID())
	require.ErrorIs(t, err, db.ErrFederationPushPending)
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.True(t, binding.PushEnabled, "refusal keeps pushing the unaccepted change")

	accepted, err := store.MaxLocalOriginEventID(ctx, project.ID)
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, accepted))

	now := time.Now().UTC()
	otherInstance, err := uid.New()
	require.NoError(t, err)
	otherClaim, err := uid.New()
	require.NoError(t, err)
	require.NoError(t, store.UpsertClaimCache(ctx, db.IssueClaim{
		ClaimUID: otherClaim, ProjectID: project.ID, IssueUID: issue.UID,
		Holder: "someone-else", HolderInstanceUID: otherInstance, ClientKind: "cli",
		ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1,
	}))
	ownClaim, err := uid.New()
	require.NoError(t, err)
	second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "claimed here", Author: "bound-agent"})
	require.NoError(t, err)
	accepted, err = store.MaxLocalOriginEventID(ctx, project.ID)
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, accepted))
	require.NoError(t, store.UpsertClaimCache(ctx, db.IssueClaim{
		ClaimUID: ownClaim, ProjectID: project.ID, IssueUID: second.UID,
		Holder: "bound-agent", HolderInstanceUID: otherInstance, ClientKind: "agent",
		ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1,
	}))
	_, err = store.DisableFederationPushWhenSettled(ctx, project.ID, store.InstanceUID())
	require.ErrorIs(t, err, db.ErrFederationClaimsHeld, "a live claim held through this replica keeps push on")
	require.NoError(t, store.ApplyClaimStatus(ctx, project.ID, second.UID, db.ClaimStatus{Held: false, HubNow: now.Add(time.Second)}))

	pending, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID, IssueRef: second.UID,
		Principal: db.ClaimPrincipal{HolderInstanceUID: store.InstanceUID(), Holder: "bound-agent", ClientKind: "cli"},
		ClaimKind: "hard",
	})
	require.NoError(t, err)
	_, err = store.DisableFederationPushWhenSettled(ctx, project.ID, store.InstanceUID())
	require.ErrorIs(t, err, db.ErrFederationClaimsHeld, "a claim request waiting for the hub keeps push on")
	require.NoError(t, store.RejectPendingClaim(ctx, pending.RequestUID, "withdrawn", now))

	before, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	disabled, err := store.DisableFederationPushWhenSettled(ctx, project.ID, store.InstanceUID())
	require.NoError(t, err, "a claim held by another replica does not belong to this one")
	assert.False(t, disabled.PushEnabled)
	assert.Equal(t, before.PushCursorEventID, disabled.PushCursorEventID)
	assert.Equal(t, before.PullCursorEventID, disabled.PullCursorEventID)
	assert.Equal(t, before.Actor, disabled.Actor)
	assert.True(t, disabled.Enabled)
	again, err := store.DisableFederationPushWhenSettled(ctx, project.ID, store.InstanceUID())
	require.NoError(t, err)
	assert.False(t, again.PushEnabled)
	for _, id := range []int64{issue.ID, second.ID} {
		_, err := store.IssueByID(ctx, id)
		require.NoError(t, err, "turning push off keeps local tasks")
	}
	return nil
}
