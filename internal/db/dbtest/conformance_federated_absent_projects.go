package dbtest

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Direct pulls retain issue history and missing move endpoints atomically.
func checkFederatedAbsentProjects(t *testing.T, hub db.Storage, backend Backend) error {
	ctx := t.Context()
	source, err := hub.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	for _, p := range []db.Project{source, target} {
		_, err = hub.EnableProjectFederation(ctx, p.ID, "worker")
		require.NoError(t, err)
	}
	issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Issue", Author: "worker"})
	require.NoError(t, err)
	moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	receiver := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, receiver.Close()) })
	route, err := receiver.CreateProjectWithUID(ctx, "spoke-project", target.UID)
	require.NoError(t, err)
	creation := portableNativeEvent(created)
	move := portableNativeEvent(*moved.Event)
	// Unbound routes cannot admit foreign history.
	_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation})
	require.Error(t, err)
	_, err = receiver.ProjectByUID(ctx, source.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = receiver.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, Actor: "worker"})
	require.NoError(t, err)
	// A later invalid event rolls the newly retained identity back with the page.
	invalid := move
	invalid.Payload = []byte(`{}`)
	resignNativeEvent(t, &invalid)
	_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation, invalid})
	require.Error(t, err)
	require.ErrorContains(t, err, invalid.EventUID)
	require.ErrorContains(t, err, invalid.ProjectUID)
	_, err = receiver.ProjectByUID(ctx, source.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	foreignProjectEvent := creation
	foreignProjectEvent.Type = "project.updated"
	foreignProjectEvent.IssueUID = nil
	resignNativeEvent(t, &foreignProjectEvent)
	_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{foreignProjectEvent})
	require.Error(t, err)
	_, err = receiver.ProjectByUID(ctx, source.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	collision, err := receiver.CreateProject(ctx, "federation-"+strings.ToLower(source.UID))
	require.NoError(t, err)
	_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation})
	require.NoError(t, err)
	retained, err := receiver.ProjectByUID(ctx, source.UID)
	require.NoError(t, err)
	require.NotNil(t, retained.DeletedAt)
	require.NotEqual(t, collision.Name, retained.Name)
	_, err = receiver.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: retained.ID, Actor: "worker"})
	require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired)
	unchanged, err := receiver.ProjectByUID(ctx, collision.UID)
	require.NoError(t, err)
	require.Equal(t, collision.Name, unchanged.Name)
	_, err = receiver.FederationBindingByProject(ctx, retained.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{move})
	require.NoError(t, err)
	require.NoError(t, receiver.MaterializeFederatedProject(ctx, route.ID))
	got, err := receiver.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, route.ID, got.ProjectID)
	other := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	otherRoute, err := other.CreateProjectWithUID(ctx, "spoke-project", target.UID)
	require.NoError(t, err)
	_, err = other.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: otherRoute.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, Actor: "worker"})
	require.NoError(t, err)
	_, err = other.InsertRemoteEvents(ctx, otherRoute.ID, []db.RemoteEvent{move})
	require.NoError(t, err)
	endpoint, err := other.ProjectByUID(ctx, source.UID)
	require.NoError(t, err)
	require.NotNil(t, endpoint.DeletedAt)
	_, err = other.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: endpoint.ID, Actor: "worker"})
	require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired)

	// A cold source route retains the outbound move even without an issue row.
	cold := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, cold.Close()) })
	coldRoute, err := cold.CreateProjectWithUID(ctx, "cold-source", source.UID)
	require.NoError(t, err)
	existing, err := cold.CreateProjectWithUID(ctx, "existing-target", target.UID)
	require.NoError(t, err)
	_, _, err = cold.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: existing.ID, Actor: "worker"})
	require.NoError(t, err)
	_, err = cold.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: coldRoute.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, Actor: "worker"})
	require.NoError(t, err)
	_, err = cold.InsertRemoteEvents(ctx, coldRoute.ID, []db.RemoteEvent{creation, move})
	require.NoError(t, err)
	require.NoError(t, cold.MaterializeFederatedProject(ctx, coldRoute.ID))
	_, err = cold.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = cold.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: existing.ID, Actor: "worker"})
	require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired)
	unrelated, err := cold.CreateProject(ctx, "unrelated-route")
	require.NoError(t, err)
	_, err = cold.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: unrelated.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: unrelated.ID, HubProjectUID: unrelated.UID, Enabled: true, Actor: "worker"})
	require.NoError(t, err)
	unrelatedCursor, err := cold.MaxEventID(ctx)
	require.NoError(t, err)
	require.NoError(t, cold.ResetFederatedProject(ctx, unrelated.ID, unrelatedCursor+1, unrelatedCursor))
	_, err = cold.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: existing.ID, Actor: "worker"})
	require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired)
	cursor, err := cold.MaxEventID(ctx)
	require.NoError(t, err)
	require.NoError(t, cold.ResetFederatedProject(ctx, coldRoute.ID, cursor+1, cursor))
	_, err = cold.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: existing.ID, Actor: "worker"})
	require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired, "a reset must preserve stream holds after removing history")
	_, err = cold.LeaveFederationReplica(ctx, coldRoute.ID)
	require.NoError(t, err)
	_, err = cold.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: existing.ID, Actor: "worker"})
	require.NoError(t, err)

	// A locally edited source projection must disappear after the outbound move,
	// even though its local event refers to the projection by numeric ID.
	edited := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, edited.Close()) })
	editedRoute, err := edited.CreateProjectWithUID(ctx, "edited-source", source.UID)
	require.NoError(t, err)
	_, err = edited.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: editedRoute.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	_, err = edited.InsertRemoteEvents(ctx, editedRoute.ID, []db.RemoteEvent{creation})
	require.NoError(t, err)
	require.NoError(t, edited.MaterializeFederatedProject(ctx, editedRoute.ID))
	local, err := edited.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	title := "Local edit"
	_, edit, _, err := edited.EditIssue(ctx, db.EditIssueParams{IssueID: local.ID, Title: &title, Actor: "worker"})
	require.NoError(t, err)
	require.NotNil(t, edit.IssueID)
	_, err = edited.InsertRemoteEvents(ctx, editedRoute.ID, []db.RemoteEvent{move})
	require.NoError(t, err)
	applyCtx, resets := db.WithFederationMoveResetCapture(ctx)
	require.NoError(t, edited.MaterializeFederatedProject(applyCtx, editedRoute.ID))
	var sourceReset bool
	for _, reset := range resets.Resets {
		sourceReset = sourceReset || reset.ProjectID == editedRoute.ID
	}
	require.True(t, sourceReset, "the visible source snapshot must be invalidated")
	listed, err := edited.ListIssues(ctx, db.ListIssuesParams{ProjectID: editedRoute.ID, Limit: 100})
	require.NoError(t, err)
	require.Empty(t, listed)
	_, err = edited.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
	history, err := edited.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
	require.NoError(t, err)
	var retainedEdit bool
	for _, event := range history {
		if event.UID == edit.UID {
			retainedEdit = true
			require.Equal(t, edit.ContentHash, event.ContentHash)
			require.Equal(t, edit.IssueUID, event.IssueUID)
		}
	}
	require.True(t, retainedEdit)

	// Reproduce normal retained and offline claim rows on a materialized source.
	for _, kind := range []string{"released", "pending"} {
		t.Run("claim-prune-"+kind, func(t *testing.T) {
			spoke := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, spoke.Close()) })
			route, err := spoke.CreateProjectWithUID(ctx, "claim-source", source.UID)
			require.NoError(t, err)
			_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
			local, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			holder, err := uid.New()
			require.NoError(t, err)
			principal := db.ClaimPrincipal{HolderInstanceUID: holder, Holder: "worker", ClientKind: "agent"}
			now := time.Now().UTC()
			if kind == "released" {
				claim, claimErr := spoke.AcquireClaim(ctx, db.AcquireClaimParams{ProjectID: route.ID, IssueRef: local.UID, Principal: principal, ClaimKind: "hard", Now: now})
				require.NoError(t, claimErr, "fixture could not acquire claim on materialized spoke issue")
				require.True(t, claim.Granted)
				released, releaseErr := spoke.ReleaseClaim(ctx, db.ReleaseClaimParams{ProjectID: route.ID, IssueRef: local.UID, Principal: principal, Now: now.Add(time.Second)})
				require.NoError(t, releaseErr)
				require.NotNil(t, released.Claim.ReleasedAt)
			} else {
				pending, queueErr := spoke.EnqueuePendingClaim(ctx, db.PendingClaimParams{ProjectID: route.ID, IssueRef: local.UID, Principal: principal, ClaimKind: "hard", Purpose: "offline", Now: now})
				require.NoError(t, queueErr, "fixture could not enqueue offline claim on materialized spoke issue")
				require.Equal(t, issue.UID, pending.IssueUID)
			}
			_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{move})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
			_, err = spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.ErrorIs(t, err, db.ErrNotFound)
			history, err := spoke.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
			require.NoError(t, err)
			var hasCreation, hasMove bool
			for _, event := range history {
				hasCreation = hasCreation || event.UID == creation.EventUID && event.ContentHash == creation.ContentHash
				hasMove = hasMove || event.UID == move.EventUID && event.ContentHash == move.ContentHash
			}
			require.True(t, hasCreation)
			require.True(t, hasMove)
		})
	}

	// A connector mapping does not block pruning the former projection.
	guarded := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, guarded.Close()) })
	guardedRoute, err := guarded.CreateProjectWithUID(ctx, "guarded-source", source.UID)
	require.NoError(t, err)
	_, err = guarded.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: guardedRoute.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	_, err = guarded.InsertRemoteEvents(ctx, guardedRoute.ID, []db.RemoteEvent{creation})
	require.NoError(t, err)
	require.NoError(t, guarded.MaterializeFederatedProject(ctx, guardedRoute.ID))
	guardedIssue, err := guarded.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = guarded.UpsertImportMapping(ctx, db.ImportMappingParams{Source: "connector", ObjectType: "issue", ExternalID: "external-issue", ProjectID: guardedRoute.ID, IssueID: &guardedIssue.ID})
	require.NoError(t, err)
	_, err = guarded.InsertRemoteEvents(ctx, guardedRoute.ID, []db.RemoteEvent{move})
	require.NoError(t, err)
	require.NoError(t, guarded.MaterializeFederatedProject(ctx, guardedRoute.ID))
	_, err = guarded.ImportMappingBySource(ctx, guardedRoute.ID, "connector", "issue", "external-issue")
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = guarded.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
	guardedHistory, err := guarded.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
	require.NoError(t, err)
	var preservedMove bool
	for _, event := range guardedHistory {
		preservedMove = preservedMove || event.UID == move.EventUID && event.ContentHash == move.ContentHash
	}
	require.True(t, preservedMove)

	// Bound counterparts can be purged after their portable history is purged.
	purged := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, purged.Close()) })
	purgeSource, err := purged.CreateProjectWithUID(ctx, "purge-source", source.UID)
	require.NoError(t, err)
	purgeTarget, err := purged.CreateProjectWithUID(ctx, "purge-target", target.UID)
	require.NoError(t, err)
	for _, p := range []db.Project{purgeSource, purgeTarget} {
		_, err = purged.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
	}
	_, err = purged.InsertRemoteEvents(ctx, purgeSource.ID, []db.RemoteEvent{creation})
	require.NoError(t, err)
	require.NoError(t, purged.MaterializeFederatedProject(ctx, purgeSource.ID))
	_, err = purged.InsertRemoteEvents(ctx, purgeSource.ID, []db.RemoteEvent{move})
	require.NoError(t, err)
	require.NoError(t, purged.MaterializeFederatedProject(ctx, purgeSource.ID))
	_, err = purged.LeaveFederationReplica(ctx, purgeTarget.ID)
	require.NoError(t, err)
	purgeIssue, err := purged.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = purged.PurgeIssue(ctx, purgeIssue.ID, "worker", nil)
	require.NoError(t, err)
	_, _, err = purged.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: purgeTarget.ID, Actor: "worker"})
	require.NoError(t, err)
	_, err = purged.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: purgeTarget.ID, Actor: "worker"})
	require.NoError(t, err)

	// A move that cannot rehome a claimed external root reports its own envelope.
	from, to, pinned, blocked := federatedRehomeFixture(t, hub, "apply-error")
	prepareRehomePull(t, hub, from, to, "pull")
	root, _, err := hub.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{ProjectID: from.ID, IssueID: pinned.ID, ConnectorInstance: "notes", ExternalRootKey: "root", ExternalAccountKey: "account", Actor: "worker", ReceiveCommentsAfter: time.Now().UTC()})
	require.NoError(t, err)
	_, _, err = hub.ClaimExternalRootBinding(ctx, root.ID, "bridge-worker", time.Now().UTC(), time.Now().UTC().Add(-db.ExternalRootClaimStaleAfter))
	require.NoError(t, err)
	_, err = hub.InsertRemoteEvents(ctx, to.ID, []db.RemoteEvent{blocked})
	require.NoError(t, err)
	err = hub.MaterializeFederatedProject(ctx, to.ID)
	require.ErrorIs(t, err, db.ErrExternalRootClaimActive)
	require.ErrorContains(t, err, blocked.EventUID)
	require.ErrorContains(t, err, blocked.ProjectUID)
	return nil
}
