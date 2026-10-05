package jsonl

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
)

func artifactBackupStore(t *testing.T, backend string) db.Storage {
	t.Helper()
	var store db.Storage
	var err error
	if backend == "sqlite" {
		store, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "backup.db"))
	} else {
		dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
		t.Cleanup(cleanup)
		store, err = pgstore.Open(t.Context(), dsn)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// R7/A13: owner backup and project transfer retain complete original artifacts
// across both stores; legacy index embeddings remain a separate skipped format.
func TestArtifactBackupRoundTrip(t *testing.T) {
	for _, sourceBackend := range []string{"sqlite", "postgres"} {
		for _, targetBackend := range []string{"sqlite", "postgres"} {
			t.Run(sourceBackend+"_to_"+targetBackend, func(t *testing.T) {
				source := artifactBackupStore(t, sourceBackend)
				dbtest.RunEmbeddingArtifactStorage(t, source)
				ctx := t.Context()
				project, err := source.ProjectByName(ctx, "artifact-project")
				require.NoError(t, err)
				before, err := source.(db.EmbeddingArtifactStorage).EmbeddingArtifactManifests(ctx, project.UID, 32)
				require.NoError(t, err)
				require.Len(t, before, 2)
				privateProject, err := source.CreateProject(ctx, "unselected-project")
				require.NoError(t, err)
				privateIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: privateProject.ID, Title: "Unselected vector canary", Body: "Private content", Author: "assistant"})
				require.NoError(t, err)
				original, err := source.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, before[0].Digest)
				require.NoError(t, err)
				identity := original.ArtifactIdentity
				identity.ProjectUID = privateProject.UID
				identity.IssueUID = privateIssue.UID
				identity.InputHash = ""
				identity.Dimensions = 2
				privateArtifact, err := embedding.NewArtifact(identity, embedding.EmbedText(privateIssue.Title, privateIssue.Body), [][]float32{{42.5, 7.25}})
				require.NoError(t, err)
				durable, err := source.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, privateArtifact)
				require.NoError(t, err)
				require.True(t, durable)

				for _, scoped := range []bool{false, true} {
					t.Run(map[bool]string{false: "owner", true: "project"}[scoped], func(t *testing.T) {
						opts := ExportOptions{IncludeDeleted: true}
						if scoped {
							opts.ProjectID = project.ID
						}
						var backup bytes.Buffer
						require.NoError(t, Export(ctx, source, &backup, opts))
						if scoped {
							for _, canary := range []string{privateProject.UID, privateIssue.UID, privateArtifact.Digest, base64.StdEncoding.EncodeToString(privateArtifact.Chunks[0].VectorBytes)} {
								require.NotContains(t, backup.String(), canary, "unselected artifact data must not enter project export")
							}
						}

						restored := artifactBackupStore(t, targetBackend)
						require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), restored))
						after, err := restored.(db.EmbeddingArtifactStorage).EmbeddingArtifactManifests(ctx, project.UID, 32)
						require.NoError(t, err)
						require.Equal(t, before, after, "original portable manifests must survive export/restore")
						for _, manifest := range before {
							original, err := source.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, manifest.Digest)
							require.NoError(t, err)
							retained, err := restored.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, manifest.Digest)
							require.NoError(t, err)
							require.Equal(t, original, retained, "original float32 bytes, including above-index dimensions, survive restore")
						}
					})
				}
			})
		}
	}
}

func TestArtifactLegacyExportRoundTrip(t *testing.T) {
	source, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	dbtest.RunEmbeddingArtifactStorage(t, source)
	ctx := t.Context()
	project, err := source.ProjectByName(ctx, "artifact-project")
	require.NoError(t, err)
	before, err := source.EmbeddingArtifactManifests(ctx, project.UID, 32)
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, exportForCutover(ctx, source, &backup, ExportOptions{IncludeDeleted: true}))
	target := artifactBackupStore(t, "sqlite")
	require.NoError(t, Import(ctx, &backup, target))
	after, err := target.(db.EmbeddingArtifactStorage).EmbeddingArtifactManifests(ctx, project.UID, 32)
	require.NoError(t, err)
	require.Equal(t, before, after, "future cutovers must use the version-gated portable-artifact export")
}

// R7/A13: malformed portable bytes fail before any target replacement.
func TestArtifactRestoreRejectsTamper(t *testing.T) {
	source := artifactBackupStore(t, "sqlite")
	dbtest.RunEmbeddingArtifactStorage(t, source)
	var backup bytes.Buffer
	require.NoError(t, Export(t.Context(), source, &backup, ExportOptions{IncludeDeleted: true}))
	records, err := NewDecoder(&backup).ReadAll(t.Context())
	require.NoError(t, err)
	changed := false
	for i := range records {
		if records[i].Kind != KindEmbeddingArtifact {
			continue
		}
		var artifact db.EmbeddingArtifactExport
		require.NoError(t, json.Unmarshal(records[i].Data, &artifact))
		artifact.Chunks[0].VectorBytes[0] ^= 1
		records[i].Data, err = json.Marshal(artifact)
		require.NoError(t, err)
		changed = true
		break
	}
	require.True(t, changed)
	var corrupt bytes.Buffer
	for _, record := range records {
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		corrupt.Write(raw)
		corrupt.WriteByte('\n')
	}
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			target := artifactBackupStore(t, backend)
			project, err := target.CreateProject(t.Context(), "existing-project")
			require.NoError(t, err)
			issue, _, err := target.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Keep target", Author: "assistant"})
			require.NoError(t, err)
			require.Error(t, Import(t.Context(), bytes.NewReader(corrupt.Bytes()), target))
			retained, err := target.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, "Keep target", retained.Title)
		})
	}
}
