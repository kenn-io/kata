package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFormerInstanceIdentities(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "restore-source")
	require.NoError(t, err)
	_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "restore", Author: "operator"})
	require.NoError(t, err)
	var records []db.ImportRecord
	for row, err := range source.ExportMeta(ctx) {
		require.NoError(t, err)
		records = append(records, &row)
	}
	for row, err := range source.ExportProjects(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		records = append(records, &row)
	}
	for row, err := range source.ExportIssues(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		records = append(records, &row)
	}
	for row, err := range source.ExportEvents(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		records = append(records, &row)
	}
	first := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	require.NoError(t, first.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
	aliases, err := first.FormerInstanceUIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{source.InstanceUID()}, aliases)
	var meta []db.ImportRecord
	for row, err := range first.ExportMeta(ctx) {
		require.NoError(t, err)
		meta = append(meta, &row)
	}
	second := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	require.NoError(t, second.ImportReplay(ctx, meta, db.ImportOptions{NewInstance: true}))
	aliases, err = second.FormerInstanceUIDs(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{source.InstanceUID(), first.InstanceUID()}, aliases)
	ordinary := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, ordinary.Close()) })
	require.NoError(t, ordinary.ImportReplay(ctx, records, db.ImportOptions{}))
	aliases, err = ordinary.FormerInstanceUIDs(ctx)
	require.NoError(t, err)
	require.Empty(t, aliases)
	return nil
}

func checkRestoredRetainedHistory(t *testing.T, _ db.Storage, backend Backend) error {
	for _, action := range []string{"reset", "guarded reset", "snapshot", "same actor snapshot", "former snapshot", "rejoin"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			source := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, source.Close()) })
			project, err := source.CreateProject(ctx, "restore-retained")
			require.NoError(t, err)
			var issues []db.Issue
			var events []db.Event
			for i := range 2 {
				issue, event, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("task %d", i), Author: "old-actor"})
				require.NoError(t, err)
				issues, events = append(issues, issue), append(events, event)
			}
			binding, err := source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
			require.NoError(t, err)
			require.NoError(t, source.AdvanceFederationPushCursor(ctx, project.ID, events[1].ID))
			var formerSnapshot db.Event
			if action == "former snapshot" {
				formerSnapshot, _, err = source.RetainedIssueSnapshot(ctx, project.ID, issues[0].UID)
				require.NoError(t, err)
			}
			records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
			require.NoError(t, err)
			restored := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, restored.Close()) })
			require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
			if action == "rejoin" {
				_, err = restored.LeaveFederationReplica(ctx, project.ID)
				require.NoError(t, err)
				binding, err = restored.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
				require.NoError(t, err)
				require.GreaterOrEqual(t, binding.RetainedThroughEventID, events[1].ID)
			}
			if action == "same actor snapshot" {
				binding.Actor = "old-actor"
				_, err = restored.UpsertFederationBinding(ctx, binding)
				require.NoError(t, err)
			}
			if action == "snapshot" || action == "same actor snapshot" || action == "former snapshot" {
				for i, issue := range issues {
					snapshot, sourceID, err := restored.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
					require.NoError(t, err)
					require.Equal(t, events[i].ID, sourceID)
					require.Equal(t, restored.InstanceUID(), snapshot.OriginInstanceUID)
					if action == "former snapshot" && i == 0 {
						require.NotEqual(t, formerSnapshot.UID, snapshot.UID)
						old, err := restored.EventsByUIDs(ctx, project.ID, []string{formerSnapshot.UID})
						require.NoError(t, err)
						require.Len(t, old, 1)
						require.Equal(t, formerSnapshot.ContentHash, old[0].ContentHash)
					}
				}
				return
			}
			if action == "guarded reset" {
				err = restored.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 1, 0, restored.InstanceUID(), events[1].ID)
			} else {
				err = restored.ResetFederatedProject(ctx, project.ID, 1, 0)
			}
			require.NoError(t, err)
			for i, issue := range issues {
				retained, err := restored.EventsByUIDs(ctx, project.ID, []string{events[i].UID})
				require.NoError(t, err)
				require.Len(t, retained, 1)
				require.Equal(t, events[i].OriginInstanceUID, retained[0].OriginInstanceUID)
				require.Equal(t, events[i].ContentHash, retained[0].ContentHash)
				_, err = restored.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
			}
		})
	}
	return nil
}
