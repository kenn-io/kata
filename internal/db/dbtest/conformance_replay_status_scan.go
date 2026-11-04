package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkSnapshotReplayRejectsMalformedRetainedStatusScan(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	for _, tc := range []struct{ name, config, uid string }{
		{"reversed cursor", `{"status_sync":"two-way","_status_sync":{"pending":{"after":3,"through":2}}}`, "01HZZZZZZZZZZZZZZZZZZZZZ21"},
		{"unknown member", `{"status_sync":"two-way","_status_sync":{"unexpected":true}}`, "01HZZZZZZZZZZZZZZZZZZZZZ22"},
		{"unknown cursor member", `{"status_sync":"two-way","_status_sync":{"sweep":{"after":0,"through":2,"unexpected":true}}}`, "01HZZZZZZZZZZZZZZZZZZZZZ23"},
		{"null checkpoint", `{"status_sync":"two-way","_status_sync":null}`, "01HZZZZZZZZZZZZZZZZZZZZZ24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, err := store.CreateProject(ctx, "example-original-project")
			require.NoError(t, err)
			records := []db.ImportRecord{
				&db.ProjectExport{ID: 41, UID: tc.uid, Name: "example-replayed-project", CreatedAt: "2026-09-29T00:00:00Z"},
				&db.IssueSyncBindingExport{ID: 71, ProjectID: 41, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(tc.config), Enabled: true, IntervalSeconds: 300, CreatedAt: "2026-09-29T00:00:00Z", UpdatedAt: "2026-09-29T00:00:00Z"},
			}
			err = store.ImportReplay(ctx, records, db.ImportOptions{PreserveIssueSyncBindingEnabled: true})
			require.ErrorIs(t, err, db.ErrImportValidation)
			retained, err := store.ProjectByUID(ctx, original.UID)
			require.NoError(t, err, "failed trusted replay must retain the original project")
			require.Equal(t, original.ID, retained.ID)
			_, err = store.ProjectByUID(ctx, tc.uid)
			require.ErrorIs(t, err, db.ErrNotFound)
			// An ordinary restore drops private scan state rather than retaining it.
			require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{}))
			binding, err := store.IssueSyncBindingByProject(ctx, 41)
			require.NoError(t, err)
			require.False(t, binding.Enabled)
			require.JSONEq(t, `{"status_sync":"two-way"}`, string(binding.Config))
		})
	}
	valid := jsontext.Value(`{"status_sync":"two-way","_status_sync":{"pending":{"after":2,"through":7},"sweep":{"after":0,"through":7}}}`)
	retainedRecords := []db.ImportRecord{
		&db.ProjectExport{ID: 41, UID: "01HZZZZZZZZZZZZZZZZZZZZZ25", Name: "example-replayed-project", CreatedAt: "2026-09-29T00:00:00Z"},
		&db.IssueSyncBindingExport{ID: 71, ProjectID: 41, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: valid, Enabled: true, IntervalSeconds: 300, CreatedAt: "2026-09-29T00:00:00Z", UpdatedAt: "2026-09-29T00:00:00Z"},
	}
	require.NoError(t, store.ImportReplay(ctx, retainedRecords, db.ImportOptions{PreserveIssueSyncBindingEnabled: true}))
	binding, err := store.IssueSyncBindingByProject(ctx, 41)
	require.NoError(t, err)
	require.True(t, binding.Enabled)
	require.JSONEq(t, string(valid), string(binding.Config))
	return nil
}

func checkSnapshotReplayPreservesPublicationBaseline(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	const projectID, bindingID, mappingID = int64(5), int64(7), int64(8)
	issueID := int64(6)
	const importedAt = "2026-09-29T00:00:00.000Z"
	const baselineHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	config := jsontext.Value(`{"project_id":"project-1","status_sync":"one-way","_status_sync":{"pending":{"after":0,"through":0},"sweep":{"after":0,"through":0}},"_provider_checkpoint":{"versions":{"task-1":{"hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","first_seen":"2026-09-29T00:00:00Z","version":"2026-09-29T00:00:00Z"}},"missing_after":"task-1","publication_content_baselines":{"task:task-1":{"hash":"` + baselineHash + `","at":"2026-09-29T00:00:00Z"}}}}`)
	issue := &db.IssueExport{
		ID: issueID, UID: replayIssueUID, ProjectID: projectID, ShortID: "zz12",
		Title: "Locally edited title", Body: "Locally edited body", Status: "open",
		Author: "fixture-author", CreatedAt: importedAt, UpdatedAt: importedAt,
		Metadata: jsontext.Value(`{}`), Revision: 2, ContentRevision: 2,
	}
	records := []db.ImportRecord{
		&db.ProjectExport{ID: projectID, UID: replayProjectUID, Name: "replay-project", CreatedAt: importedAt, Metadata: jsontext.Value(`{}`), Revision: 1},
		issue,
		&db.IssueSyncBindingExport{
			ID: bindingID, ProjectID: projectID, Provider: "ticktick", SourceKey: "ticktick:project-1", RemoteID: "project-1",
			DisplayName: "Example tasks", Config: config, Enabled: true, IntervalSeconds: 300, CreatedAt: importedAt, UpdatedAt: importedAt,
		},
		&db.ImportMappingExport{ID: mappingID, Source: "ticktick:project-1", ExternalID: "task:task-1", ObjectType: "issue", ProjectID: projectID, IssueID: &issueID, ImportedAt: importedAt},
	}
	if err := store.ImportReplay(ctx, records, db.ImportOptions{}); err != nil {
		return err
	}
	binding, err := store.IssueSyncBindingByProject(ctx, projectID)
	require.NoError(t, err)
	require.False(t, binding.Enabled, "ordinary restores must disable external sync")
	require.JSONEq(t, `{"project_id":"project-1","status_sync":"one-way","_provider_checkpoint":{"publication_content_baselines":{"task:task-1":{"hash":"`+baselineHash+`","at":"2026-09-29T00:00:00Z"}}}}`, string(binding.Config),
		"restore may retain the inert publication baseline, not scan progress or task checkpoints")
	mapping, err := store.ImportMappingBySource(ctx, projectID, "ticktick:project-1", "issue", "task:task-1")
	require.NoError(t, err)
	require.Nil(t, mapping.SourceUpdatedAt, "the first content import must remain pending after restore")
	gotIssue, err := store.IssueByID(ctx, issueID)
	require.NoError(t, err)
	require.Equal(t, issue.Title, gotIssue.Title)
	require.Equal(t, issue.Body, gotIssue.Body)
	pending, err := store.(db.IssuePublicationStore).PendingIssuePublication(ctx, projectID)
	require.NoError(t, err)
	require.Nil(t, pending, "ordinary restore must not resume an executable publication attempt")
	return nil
}
