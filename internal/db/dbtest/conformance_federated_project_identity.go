package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

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
	t.Run("historical source remains joined", func(t *testing.T) {
		for _, operation := range []string{"purge", "merge"} {
			ctx := t.Context()
			source, transit, issue, first := federatedRehomeFixture(t, store, "transit-identity-"+operation)
			prepareRehomePull(t, store, source, transit, "pull")
			replayFederatedRehome(t, store, transit, first, "pull")
			target, err := store.CreateProject(ctx, "final-identity-"+operation)
			require.NoError(t, err)
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Actor: "worker", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			current, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			at := time.UnixMilli(first.HLCPhysicalMS + 1000).UTC()
			raw, err := json.Marshal(map[string]any{
				"issue_uid": issue.UID, "from_project_uid": transit.UID, "to_project_uid": target.UID,
				"from_short_id": current.ShortID, "to_short_id": current.ShortID, "updated_at": at.Format(time.RFC3339Nano),
				"snapshot": map[string]string{"uid": current.UID, "short_id": current.ShortID, "title": current.Title, "body": current.Body, "author": current.Author, "status": current.Status},
			})
			require.NoError(t, err)
			second := newRemoteEvent(t, target, &issue.UID, "issue.moved", "worker", first.OriginInstanceUID, first.HLCPhysicalMS+1000, jsontext.Value(raw))
			replayFederatedRehome(t, store, target, second, "pull")
			for _, project := range []db.Project{transit, target} {
				_, err = store.LeaveFederationReplica(ctx, project.ID)
				require.NoError(t, err)
			}
			mergeTarget, err := store.CreateProject(ctx, "transit-merge-target-"+operation)
			require.NoError(t, err)
			if operation == "purge" {
				_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: transit.ID, Actor: "worker"})
				require.NoError(t, err)
			}
			cleanup := func() error {
				if operation == "purge" {
					_, err := store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: transit.ID, Actor: "worker"})
					return err
				}
				_, err := store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: transit.ID, TargetProjectID: mergeTarget.ID, Actor: "worker"})
				return err
			}
			require.ErrorContains(t, cleanup(), "identity is required by federation history")
			require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID))
			_, err = store.LeaveFederationReplica(ctx, source.ID)
			require.NoError(t, err)
			require.NoError(t, cleanup(), "only an unrelated bound project can remain after source teardown")
		}
	})
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
