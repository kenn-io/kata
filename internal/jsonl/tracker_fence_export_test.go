package jsonl_test

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestTrackerExecutionStateIsFullBackupOnlyAndRestoresInactive(t *testing.T) {
	source, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(t.Context(), "tracker-project")
	require.NoError(t, err)
	fence := db.TrackerExecutionFence{
		Version: 1, Role: "coordinator", RootInstanceUID: source.InstanceUID(), ProjectUID: project.UID,
		Connection: db.IssueSyncConnection{OwnerInstanceUID: source.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"},
		Sequence:   1, PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", SourceKey: "notion:example-source", Locator: "example-item", Target: "closed",
		ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Phase: db.TrackerWritePrepared,
	}
	fence.WriteUID, err = db.TrackerStatusWriteUID(fence.RootInstanceUID, fence.ProjectUID, fence.SourceKey, fence.Locator, fence.PendingEventUID, fence.Target)
	require.NoError(t, err)
	require.NoError(t, source.StoreTrackerExecutionFence(t.Context(), nil, fence, fence.ProcessEpoch))
	for _, legacy := range []bool{false, true} {
		var exportStore db.Storage = source
		if legacy {
			exportStore = spokeNamesLegacyExportStore{source}
		}
		for _, scoped := range []bool{false, true} {
			opts := jsonl.ExportOptions{}
			if scoped {
				opts.ProjectID = project.ID
			}
			var data bytes.Buffer
			require.NoError(t, jsonl.Export(t.Context(), exportStore, &data, opts))
			records, err := jsonl.NewDecoder(bytes.NewReader(data.Bytes())).ReadAll(t.Context())
			require.NoError(t, err)
			count := 0
			for _, record := range records {
				if record.Kind != jsonl.KindMeta {
					continue
				}
				var meta db.MetaKV
				require.NoError(t, json.Unmarshal(record.Data, &meta))
				if strings.HasPrefix(meta.Key, db.TrackerMetadataPrefix) {
					count++
				}
			}
			if scoped {
				require.Zero(t, count)
				continue
			}
			require.Equal(t, 1, count)
			target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(data.Bytes()), target, jsonl.ImportOptions{}))
			got, found, err := target.LoadTrackerExecutionFence(t.Context(), fence.RootInstanceUID, project.UID, fence.Role)
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, got.ActivationRequired)
			require.Equal(t, db.TrackerWriteUnknown, got.Phase)
		}
	}
}

func TestNamedBindingRestoreInstallsBarrierWithoutAnyExecutionFence(t *testing.T) {
	source, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(t.Context(), "tracker-project")
	require.NoError(t, err)
	selection := db.IssueSyncConnection{OwnerInstanceUID: source.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
	_, err = source.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source, &data, jsonl.ExportOptions{}))
	target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(data.Bytes()), target, jsonl.ImportOptions{PreserveIssueSyncBindingEnabled: true}))
	barrier, found, err := target.LoadTrackerActivation(t.Context(), project.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, barrier.Active)
	require.Equal(t, selection, barrier.Connection)
}
