package vector_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
)

// A root that selected itself as a shared project's embedding producer pins
// the recipe it was selected with. After the root changes its own model, or a
// recipe version change invalidates the old selection, the root must record
// its current recipe and keep embedding its own shared content instead of
// leaving the project without vectors until an operator patches metadata.
func TestEmbeddingRootSelfSelectionFollowsRecipeChange(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"root_model_change", "root_recipe_version"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
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
				}
				var inputs atomic.Int64
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Input []string `json:"input"`
					}
					require.NoError(t, json.UnmarshalRead(r.Body, &request))
					inputs.Add(int64(len(request.Input)))
					data := []map[string]any{}
					for i := range request.Input {
						data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0}})
					}
					require.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
				}))
				t.Cleanup(provider.Close)
				modelA, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model-a", Dims: 2})
				require.NoError(t, err)
				modelB, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model-b", Dims: 2})
				require.NoError(t, err)

				private, err := source.CreateProject(ctx, "private-project")
				require.NoError(t, err)
				_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: private.ID, Title: "Private content", Author: "member"})
				require.NoError(t, err)
				shared, err := source.CreateProject(ctx, "hub-project")
				require.NoError(t, err)
				_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: shared.ID, Role: db.FederationRoleHub, HubProjectID: shared.ID, HubProjectUID: shared.UID, Enabled: true})
				require.NoError(t, err)
				public, _, err := ed25519.GenerateKey(nil)
				require.NoError(t, err)
				require.NoError(t, source.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: shared.UID, AuthorityUID: source.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: shared.ID, Title: "Shared content", Author: "member"})
				require.NoError(t, err)

				health := runReconcilerOnce(t, source, ix, modelA)
				require.Zero(t, health.Backlog, "the root selects itself and embeds under model A")
				project, err := source.ProjectByID(ctx, shared.ID)
				require.NoError(t, err)
				selected, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
				require.NoError(t, err)
				require.NotNil(t, selected, "the root selected itself under model A")
				require.Equal(t, source.InstanceUID(), selected.ProducerInstanceUID)
				if mode == "root_recipe_version" {
					selected.Recipe.RecipeVersion = embedding.RecipeVersion - 1
					writeRawProjectMetadata(t, source, shared.ID, selected)
				}

				inputs.Store(0)
				for range 3 {
					health = runReconcilerOnce(t, source, ix, modelB)
				}
				require.EqualValues(t, 2, inputs.Load(), "the root embeds its own shared issue with model B")
				require.Zero(t, health.Backlog)
				project, err = source.ProjectByID(ctx, shared.ID)
				require.NoError(t, err)
				selected, err = db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
				require.NoError(t, err)
				require.NotNil(t, selected)
				recipeB, err := modelB.ArtifactIdentity("", "", "")
				require.NoError(t, err)
				require.Equal(t, source.InstanceUID(), selected.ProducerInstanceUID)
				require.Equal(t, recipeB, selected.Recipe, "the root's own selection follows its configured recipe")
			})
		}
	}
}

func runReconcilerOnce(t *testing.T, source db.Storage, ix *vector.Index, client *embedding.Client) daemon.ReconcilerHealth {
	t.Helper()
	reconciler := daemon.NewReconciler(source, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour})
	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(runCtx) }()
	//nolint:kennlint // sleeptest: Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
	require.Eventually(t, func() bool { return reconciler.Health().LastSuccessAt != nil }, 10*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("embedding worker did not stop")
	}
	return reconciler.Health()
}

// writeRawProjectMetadata stores a selection this build no longer accepts, as
// a retained owner backup or an older release could have written it.
func writeRawProjectMetadata(t *testing.T, source db.Storage, projectID int64, producer *db.ProjectEmbeddingProducer) {
	t.Helper()
	raw, err := json.Marshal(producer)
	require.NoError(t, err)
	blob, err := json.Marshal(map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw})
	require.NoError(t, err)
	if pg, ok := source.(*pgstore.Store); ok {
		_, err = pg.ExecContext(t.Context(), `UPDATE projects SET metadata=$1 WHERE id=$2`, string(blob), projectID)
	} else {
		_, err = source.(*sqlitestore.Store).ExecContext(t.Context(), `UPDATE projects SET metadata=? WHERE id=?`, string(blob), projectID)
	}
	require.NoError(t, err)
}
