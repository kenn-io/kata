package vector_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// ReconcileArtifacts only looks at the first pending page, so a retained
// artifact for a later document waits behind older pending ones.
// ReconcileArtifactsFor imports the documents it is asked about regardless.
func TestReconcileArtifactsForImportsRequestedDocumentsBeyondFirstPage(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var source db.Storage
			var ix *vector.Index
			if backend == "sqlite" {
				s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
				source = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, ix.Close()) })
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				source = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				ix, err = vector.OpenPostgres(ctx, s.DB)
				require.NoError(t, err)
				release, err := ix.AcquireReconcilerLease(ctx)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, release()) })
			}
			project, err := source.CreateProject(ctx, "spoke-project")
			require.NoError(t, err)
			var last db.Issue
			for i := range 260 {
				last, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("document %03d", i), Author: "member"})
				require.NoError(t, err)
			}
			_, err = ix.RefreshMirror(ctx, source)
			require.NoError(t, err)
			client, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:1", Model: "example-model", Dims: 2})
			require.NoError(t, err)
			recipe, err := client.ArtifactIdentity("", "", source.InstanceUID())
			require.NoError(t, err)
			key := client.Generation().Fingerprint()
			require.NoError(t, ix.EnsureBuilding(ctx, key, client.Generation()))

			identity := recipe
			identity.ProjectUID, identity.IssueUID = project.UID, last.UID
			identity.ProducerInstanceUID = "00000000000000000000000002"
			artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(last.Title, last.Body), [][]float32{{1, 0}})
			require.NoError(t, err)
			durable, err := source.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
			require.NoError(t, err)
			require.True(t, durable)

			reused, err := ix.ReconcileArtifacts(ctx, key, source, recipe)
			require.NoError(t, err)
			require.Zero(t, reused, "the newest document is beyond the first pending page")

			reused, err = ix.ReconcileArtifactsFor(ctx, key, source, recipe, []string{"unknown-uid", last.UID})
			require.NoError(t, err)
			require.Equal(t, 1, reused)
			hits, err := ix.Query(ctx, key, kitvec.Vector{1, 0}, 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, last.UID, hits[0].Doc)

			reused, err = ix.ReconcileArtifactsFor(ctx, key, source, recipe, []string{last.UID})
			require.NoError(t, err)
			require.Zero(t, reused, "a covered document is not pending")
		})
	}
}
