package jsonl

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRejoinRetainedHistoryBackup(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, old, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "offline task", Author: "old-actor"})
	require.NoError(t, err)
	_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "snapshot"}[legacy], func(t *testing.T) {
			var backup bytes.Buffer
			if legacy {
				err = exportSnapshot(ctx, source, &backup, ExportOptions{})
			} else {
				err = Export(ctx, source, &backup, ExportOptions{})
			}
			require.NoError(t, err)
			restored, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = restored.Close() })
			require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), restored))
			binding, err := restored.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			require.GreaterOrEqual(t, binding.RetainedThroughEventID, old.ID)
		})
	}
}
