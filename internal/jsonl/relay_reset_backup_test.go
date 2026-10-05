package jsonl

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

// R4/R5: owner backups retain exact checkpoint and historical key bytes after
// compaction and rotation. Corruption fails before SQLite/PostgreSQL clearing.
func TestRelayResetBackupAndValidation(t *testing.T) {
	for _, cutover := range []bool{false, true} {
		name := "current_storage"
		if cutover {
			name = "forward_cutover"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = source.Close() })
			dbtest.RunCreateRelayResetAfterCompaction(t, source, func(ctx context.Context, projectID int64) error {
				_, err := source.ExecContext(ctx, `DELETE FROM events WHERE project_id=?`, projectID)
				return err
			})
			project, err := source.ProjectByName(ctx, "reset-root-project")
			require.NoError(t, err)
			expected, err := source.RootAuthority(ctx, project.UID)
			require.NoError(t, err)
			var backup bytes.Buffer
			if cutover {
				err = exportForCutover(ctx, source, &backup, ExportOptions{IncludeDeleted: true})
			} else {
				err = Export(ctx, source, &backup, ExportOptions{IncludeDeleted: true})
			}
			require.NoError(t, err)
			envelopes, err := NewDecoder(bytes.NewReader(backup.Bytes())).ReadAll(ctx)
			require.NoError(t, err)
			var corrupt bytes.Buffer
			encoder := NewEncoder(&corrupt)
			changed := false
			for _, envelope := range envelopes {
				if envelope.Kind == KindMeta {
					var metadata db.MetaKV
					require.NoError(t, json.Unmarshal(envelope.Data, &metadata))
					if strings.HasPrefix(metadata.Key, db.RelayResetMetadataPrefix) {
						var checkpoint db.RelayResetCheckpoint
						require.NoError(t, json.Unmarshal([]byte(metadata.Value), &checkpoint))
						checkpoint.Manifest.Signature[0] ^= 1
						raw, err := json.Marshal(checkpoint)
						require.NoError(t, err)
						metadata.Value = string(raw)
						envelope.Data, err = json.Marshal(metadata)
						require.NoError(t, err)
						changed = true
					}
				}
				require.NoError(t, encoder.Write(envelope))
			}
			require.True(t, changed)
			dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
			t.Cleanup(cleanup)
			postgres, err := pgstore.Open(ctx, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = postgres.Close() })
			sqlite, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlite.Close() })
			for index, target := range []db.Storage{postgres, sqlite} {
				t.Run([]string{"postgres", "sqlite"}[index], func(t *testing.T) {
					require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), target))
					got, err := target.RootAuthority(ctx, project.UID)
					require.NoError(t, err)
					require.Equal(t, expected, got)
					restoredIssue, err := target.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
					require.NoError(t, err)
					verified, legacy := false, false
					for _, issue := range restoredIssue {
						if issue.Title == "Current title" {
							require.Equal(t, "verified", issue.Verification)
							require.Equal(t, "company-member", issue.AccountableActor)
							verified = true
						}
						if issue.Title == "Historical legacy issue" {
							require.Equal(t, "legacy", issue.Verification)
							legacy = true
						}
					}
					require.True(t, verified)
					require.True(t, legacy)
					sourceReset, targetReset := map[string]string{}, map[string]string{}
					for record, err := range source.ExportMeta(ctx) {
						require.NoError(t, err)
						if strings.HasPrefix(record.Key, db.RelayResetMetadataPrefix) {
							sourceReset[record.Key] = record.Value
						}
					}
					for record, err := range target.ExportMeta(ctx) {
						require.NoError(t, err)
						if strings.HasPrefix(record.Key, db.RelayResetMetadataPrefix) {
							targetReset[record.Key] = record.Value
						}
					}
					require.Equal(t, sourceReset, targetReset, "restore exact signed checkpoint and per-hop translation bytes")
					sourceRelay, targetRelay := []db.ImportRecord{}, []db.ImportRecord{}
					for record, err := range source.ExportRelayState(ctx) {
						require.NoError(t, err)
						sourceRelay = append(sourceRelay, record)
					}
					for record, err := range target.ExportRelayState(ctx) {
						require.NoError(t, err)
						targetRelay = append(targetRelay, record)
					}
					require.Equal(t, sourceRelay, targetRelay, "owner restore must retain exact delivery mappings without generating fresh receipt intent")

					sentinel, err := target.CreateProject(ctx, "keep-project")
					require.NoError(t, err)
					assert.Error(t, Import(ctx, bytes.NewReader(corrupt.Bytes()), target), "invalid signed reset must be rejected before clearing")
					retained, err := target.ProjectByName(ctx, sentinel.Name)
					require.NoError(t, err)
					require.Equal(t, sentinel, retained)
				})
			}
		})
	}
}
