package dbtest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
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

	// A caller retries after a failed provider release with push already off.
	// The old credential still forwards claims, so a claim taken in between
	// must block that retry until it is released.
	lateClaim, err := uid.New()
	require.NoError(t, err)
	require.NoError(t, store.UpsertClaimCache(ctx, db.IssueClaim{
		ClaimUID: lateClaim, ProjectID: project.ID, IssueUID: issue.UID,
		Holder: "bound-agent", HolderInstanceUID: store.InstanceUID(), ClientKind: "cli",
		ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1,
	}))
	_, err = store.DisableFederationPushWhenSettled(ctx, project.ID, store.InstanceUID())
	require.ErrorIs(t, err, db.ErrFederationClaimsHeld, "a claim taken after push stopped blocks the retry")
	require.NoError(t, store.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{Held: false, HubNow: now.Add(2 * time.Second)}))
	again, err = store.DisableFederationPushWhenSettled(ctx, project.ID, store.InstanceUID())
	require.NoError(t, err)
	assert.False(t, again.PushEnabled)
	for _, id := range []int64{issue.ID, second.ID} {
		_, err := store.IssueByID(ctx, id)
		require.NoError(t, err, "turning push off keeps local tasks")
	}
	return nil
}

// A spoke's bound actor changes only after work presented to the hub as the
// old actor is finished: accepted local changes while push is on and cron
// runs the old actor started. Refusal keeps the old actor.
func checkFederationBoundActorReplacementWaitsForWork(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitionsAs(t, store, project, "first-actor")
	runUID, err := uid.New()
	require.NoError(t, err)
	started, err := store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "first-actor", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	hubUID, err := uid.New()
	require.NoError(t, err)
	adopted, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: hubUID, Actor: "first-actor"})
	require.NoError(t, err)
	accepted, err := store.MaxLocalOriginEventID(ctx, project.ID)
	require.NoError(t, err)
	_, err = store.EnableFederationPush(ctx, project.ID, max(accepted, adopted.Binding.PushCursorEventID))
	require.NoError(t, err)

	_, err = store.ReplaceFederationBoundActorWhenSettled(ctx, project.ID, store.InstanceUID(), "first-actor", "second-actor")
	require.ErrorIs(t, err, db.ErrFederationRunsActive, "a running run must keep the actor that started it")
	finished, err := store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "first-actor", Status: "succeeded", Summary: cron.Summary{Version: 1}, ExpectedRevision: started.Run.Revision})
	require.NoError(t, err)
	require.Equal(t, "succeeded", finished.Run.Status)

	_, err = store.ReplaceFederationBoundActorWhenSettled(ctx, project.ID, store.InstanceUID(), "first-actor", "second-actor")
	require.ErrorIs(t, err, db.ErrFederationPushPending, "the finished run is not yet accepted by the hub")
	accepted, err = store.MaxLocalOriginEventID(ctx, project.ID)
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, accepted))

	_, err = store.ReplaceFederationBoundActorWhenSettled(ctx, project.ID, store.InstanceUID(), "other-actor", "second-actor")
	require.ErrorIs(t, err, db.ErrFederationBoundActorChanged)
	before, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, "first-actor", before.Actor)

	replaced, err := store.ReplaceFederationBoundActorWhenSettled(ctx, project.ID, store.InstanceUID(), "first-actor", "second-actor")
	require.NoError(t, err)
	assert.Equal(t, "second-actor", replaced.Actor)
	assert.True(t, replaced.PushEnabled)
	assert.Equal(t, before.PushCursorEventID, replaced.PushCursorEventID)
	again, err := store.ReplaceFederationBoundActorWhenSettled(ctx, project.ID, store.InstanceUID(), "first-actor", "second-actor")
	require.NoError(t, err)
	assert.Equal(t, "second-actor", again.Actor)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "after the change", Author: "someone"})
	require.NoError(t, err)
	assert.Equal(t, "second-actor", issue.Author, "later local changes carry the new bound actor")

	// A read-only binding never pushes its local events, so they do not wait.
	readOnly, err := store.CreateProject(ctx, "read-only-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: readOnly.ID, Title: "before attach", Author: "someone"})
	require.NoError(t, err)
	readOnlyHub, err := uid.New()
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: readOnly.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 43, HubProjectUID: readOnlyHub,
		ReplayHorizonEventID: 1, Actor: "first-actor", Enabled: true,
	})
	require.NoError(t, err)
	upgraded, err := store.ReplaceFederationBoundActorWhenSettled(ctx, readOnly.ID, store.InstanceUID(), "first-actor", "second-actor")
	require.NoError(t, err)
	assert.Equal(t, "second-actor", upgraded.Actor)
	assert.False(t, upgraded.PushEnabled)
	return nil
}

// Turning push off makes a spoke read-only, which cannot own an active
// external root binding. The refusal keeps pushing and keeps the binding.
func checkFederationPushDisableRefusesExternalRoot(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "bound root", Author: "bound-agent"})
	require.NoError(t, err)
	root, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "notes", ExternalRootKey: "root-key",
		ExternalAccountKey: "account-key", Actor: "bound-agent", ReceiveCommentsAfter: time.Now().UTC(),
	})
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
	accepted, err := store.MaxLocalOriginEventID(ctx, project.ID)
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, accepted))

	_, err = store.DisableFederationPushWhenSettled(ctx, project.ID, store.InstanceUID())
	require.ErrorIs(t, err, db.ErrExternalRootFederationConflict)
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.True(t, binding.PushEnabled, "refusal keeps the replica collaborating")
	kept, err := store.ExternalRootBindingByID(ctx, root.ID)
	require.NoError(t, err)
	assert.True(t, kept.Active, "refusal keeps the external root binding")
	return nil
}
