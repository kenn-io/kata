package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkSnapshotReplayRejectsMalformedRetainedStatusScan(t *testing.T, store db.Storage) error {
	ctx := context.Background()
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
