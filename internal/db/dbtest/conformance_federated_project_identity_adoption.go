package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedFormerProjectIdentityAdoption(t *testing.T, store db.Storage) error {
	t.Run("joined moved history retains former UID", func(t *testing.T) {
		ctx := t.Context()
		source, target, issue, move := federatedRehomeFixture(t, store, "adoption-identity")
		prepareRehomePull(t, store, source, target, "pull")
		replayFederatedRehome(t, store, target, move, "pull")
		_, err := store.LeaveFederationReplica(ctx, source.ID)
		require.NoError(t, err)
		unrelated, err := store.CreateProject(ctx, "unrelated-adoption-identity")
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: unrelated.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: unrelated.ID, HubProjectUID: unrelated.UID, Actor: "worker", Enabled: true})
		require.NoError(t, err)
		before, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, Limit: 100})
		require.NoError(t, err)
		beforeCursor, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		current, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		newUID, err := uid.New()
		require.NoError(t, err)
		params := db.AdoptProjectIntoFederationParams{ProjectID: source.ID, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: newUID, Actor: "worker"}
		_, err = store.AdoptProjectIntoFederation(ctx, params)
		require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired)
		retained, err := store.ProjectByUID(ctx, source.UID)
		require.NoError(t, err)
		require.Equal(t, source.ID, retained.ID)
		_, err = store.ProjectByUID(ctx, newUID)
		require.ErrorIs(t, err, db.ErrNotFound)
		_, err = store.FederationBindingByProject(ctx, source.ID)
		require.ErrorIs(t, err, db.ErrNotFound)
		after, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, Limit: 100})
		require.NoError(t, err)
		require.Equal(t, before, after, "rejected adoption must preserve original envelopes and hashes")
		afterCursor, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, beforeCursor, afterCursor)
		afterIssue, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, current, afterIssue)
		_, err = store.LeaveFederationReplica(ctx, target.ID)
		require.NoError(t, err)
		adopted, err := store.AdoptProjectIntoFederation(ctx, params)
		require.NoError(t, err, "unrelated bindings do not prevent adoption after relevant history detaches")
		require.Equal(t, newUID, adopted.Project.UID)
	})
	t.Run("ordinary moved history covered by baseline", func(t *testing.T) {
		ctx := t.Context()
		source, err := store.CreateProject(ctx, "ordinary-adoption-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, "ordinary-adoption-target")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Ordinary moved issue", Author: "worker"})
		require.NoError(t, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, target.ID, "worker")
		require.NoError(t, err)
		newUID, err := uid.New()
		require.NoError(t, err)
		adopted, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: source.ID, HubURL: "https://hub.example", HubProjectID: 43, HubProjectUID: newUID, Actor: "worker"})
		require.NoError(t, err, "a covering adoption baseline removes the old local identity dependency")
		require.Equal(t, newUID, adopted.Project.UID)
	})
	return nil
}
