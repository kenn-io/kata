package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederatedFormerProjectIdentityCleanup(t *testing.T, store db.Storage) error {
	for _, operation := range []string{"purge", "merge"} {
		for _, paused := range []bool{false, true} {
			name := operation + "-joined"
			if paused {
				name += "-paused"
			}
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				source, target, issue, move := federatedRehomeFixture(t, store, "identity-"+name)
				prepareRehomePull(t, store, source, target, "pull")
				replayFederatedRehome(t, store, target, move, "pull")
				_, err := store.LeaveFederationReplica(ctx, source.ID)
				require.NoError(t, err, "acknowledged source teardown remains allowed")
				if paused {
					binding, err := store.FederationBindingByProject(ctx, target.ID)
					require.NoError(t, err)
					binding.Enabled, binding.PushEnabled = false, false
					_, err = store.UpsertFederationBinding(ctx, binding)
					require.NoError(t, err)
				}
				unrelated, err := store.CreateProject(ctx, "unrelated-"+name)
				require.NoError(t, err)
				_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: unrelated.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: unrelated.ID, HubProjectUID: unrelated.UID, Actor: "worker", Enabled: true})
				require.NoError(t, err)
				mergeTarget, err := store.CreateProject(ctx, "standalone-"+name)
				require.NoError(t, err)
				if operation == "purge" {
					_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: source.ID, Actor: "worker"})
					require.NoError(t, err, "archive retains the historical project identity")
				}
				cleanup := func() error {
					if operation == "purge" {
						_, err := store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: source.ID, Actor: "worker"})
						return err
					}
					_, err := store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: mergeTarget.ID, Actor: "worker"})
					return err
				}
				require.ErrorContains(t, cleanup(), "identity is required by federation history")
				retained, err := store.ProjectByUID(ctx, source.UID)
				require.NoError(t, err)
				require.Equal(t, source.ID, retained.ID)
				current, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, target.ID, current.ProjectID)
				_, err = store.LeaveFederationReplica(ctx, target.ID)
				require.NoError(t, err)
				require.NoError(t, cleanup(), "standalone moved issues and unrelated bindings do not block ordinary cleanup")
			})
		}
	}
	t.Run("ordinary move before destination adoption", func(t *testing.T) {
		for _, operation := range []string{"purge", "merge"} {
			source, err := store.CreateProject(t.Context(), "ordinary-identity-source-"+operation)
			require.NoError(t, err)
			target, err := store.CreateProject(t.Context(), "ordinary-identity-target-"+operation)
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "ordinary moved issue", Author: "worker"})
			require.NoError(t, err)
			_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			_, err = store.EnableProjectFederation(t.Context(), target.ID, "worker")
			require.NoError(t, err)
			if operation == "purge" {
				_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: source.ID, Actor: "worker"})
				require.NoError(t, err)
				_, err = store.PurgeProject(t.Context(), db.PurgeProjectParams{ProjectID: source.ID, Actor: "worker"})
			} else {
				mergeTarget, createErr := store.CreateProject(t.Context(), "ordinary-identity-merge-target")
				require.NoError(t, createErr)
				_, err = store.MergeProjects(t.Context(), db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: mergeTarget.ID, Actor: "worker"})
			}
			require.NoError(t, err, "the adoption baseline covers incompatible local history, so no federation identity dependency remains")
		}
	})
	return nil
}
