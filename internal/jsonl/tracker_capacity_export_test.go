package jsonl

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/tracker"
)

func TestTrackerCapacityExcludedFromFullExports(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	receipt := tracker.ControlReceipt{Version: 1, RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, UID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Principal: tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}, Selection: tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Kind: tracker.ControlRun, SourceKey: "notion:source", RequestDigest: strings.Repeat("a", 64), State: tracker.ControlPending}
	_, fresh, err := store.BeginTrackerControl(ctx, receipt)
	require.NoError(t, err)
	require.True(t, fresh)
	var counter db.MetaKV
	for record, err := range store.ExportMeta(ctx) {
		require.NoError(t, err)
		if strings.HasPrefix(record.Key, db.TrackerMetadataPrefix+"control_capacity.") {
			counter = record
		}
	}
	require.NotEmpty(t, counter.Key)
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "storage", true: "cutover"}[legacy], func(t *testing.T) {
			var output bytes.Buffer
			if legacy {
				err = exportForCutover(ctx, store, &output, ExportOptions{})
			} else {
				err = Export(ctx, store, &output, ExportOptions{})
			}
			require.NoError(t, err)
			records, err := NewDecoder(&output).ReadAll(ctx)
			require.NoError(t, err)
			foundReceipt := false
			for _, record := range records {
				if record.Kind != KindMeta {
					continue
				}
				var meta db.MetaKV
				require.NoError(t, json.Unmarshal(record.Data, &meta))
				require.NotEqual(t, counter.Key, meta.Key)
				foundReceipt = foundReceipt || meta.Key == db.TrackerControlKey(receipt.RootInstanceUID, receipt.ProjectUID, receipt.UID)
			}
			require.True(t, foundReceipt)
		})
	}
}
