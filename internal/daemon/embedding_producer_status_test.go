package daemon_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
)

// R7: scoped status describes retained artifacts without vectors, provider
// credentials, or paid calls. A recipient without an index reports that limit.
func TestEmbeddingProducerStatusScope(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		var calls atomic.Int32
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(provider.Close)
		client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 4001, Credential: config.EmbeddingCredential{Key: "status-provider-secret"}})
		require.NoError(t, err)
		recipe, err := client.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		producer := db.ProjectEmbeddingProducer{ProducerInstanceUID: store.InstanceUID(), Recipe: recipe}
		raw, err := json.Marshal(producer)
		require.NoError(t, err)
		_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: f.private.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
		require.NoError(t, err)
		identity, err := client.ArtifactIdentity(f.private.UID, f.issue.UID, store.InstanceUID())
		require.NoError(t, err)
		vector := make([]float32, 4001)
		vector[0] = 1
		artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(f.issue.Title, f.issue.Body), [][]float32{vector})
		require.NoError(t, err)
		durable, err := store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
		require.NoError(t, err)
		require.True(t, durable)
		path := fmt.Sprintf("/api/v1/projects/%d/federation/status", f.private.ID)
		status, _, body := f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var response struct {
			Statuses []struct {
				Embedding *struct {
					Producer  *db.ProjectEmbeddingProducer `json:"producer"`
					State     string                       `json:"state"`
					Artifacts []struct {
						Digest   string `json:"digest"`
						IssueUID string `json:"issue_uid"`
						State    string `json:"state"`
					} `json:"artifacts"`
				} `json:"embedding"`
			} `json:"statuses"`
		}
		require.NoError(t, json.Unmarshal(body, &response))
		require.Len(t, response.Statuses, 1)
		require.NotNil(t, response.Statuses[0].Embedding, "existing federation status must advertise producer and artifact state")
		got := response.Statuses[0].Embedding
		require.Equal(t, &producer, got.Producer)
		require.Equal(t, "stored_unindexed", got.State)
		require.Len(t, got.Artifacts, 1)
		require.Equal(t, artifact.Digest, got.Artifacts[0].Digest)
		require.Equal(t, f.issue.UID, got.Artifacts[0].IssueUID)
		require.Equal(t, "stored_unindexed", got.Artifacts[0].State)
		require.NotContains(t, string(body), "vector_bytes")
		require.NotContains(t, string(body), "status-provider-secret")
		require.Zero(t, calls.Load())
		globalCode, _, globalBody := f.request(t, http.MethodGet, "/api/v1/federation/status", "member", nil, nil)
		require.Equal(t, http.StatusNotFound, globalCode, string(globalBody))
		require.NotContains(t, string(globalBody), artifact.Digest)
		require.NotContains(t, string(globalBody), store.InstanceUID())
		_, err = store.SetTeamMembership(ctx, f.team.UID, "member", false, "owner")
		require.NoError(t, err)
		status, _, body = f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusNotFound, status, string(body))
		require.NotContains(t, string(body), artifact.Digest)
		require.NotContains(t, string(body), f.issue.UID)
		require.NotContains(t, string(body), store.InstanceUID())
		require.Zero(t, calls.Load())
	})
}

// Retained artifacts can outlive an issue's move. They must not fill the
// bounded status page or turn a healthy federation status response into 500.
func TestEmbeddingProducerStatusSkipsMovedArtifactsBeforeLimit(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		destination, err := store.CreateProject(ctx, "artifact-destination")
		require.NoError(t, err)
		validIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: f.private.ID, Title: "Current artifact issue", Author: "member",
		})
		require.NoError(t, err)
		client, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "example-model", Dims: 2})
		require.NoError(t, err)
		issues := []db.Issue{f.issue, validIssue}
		artifacts := make([]embedding.EmbeddingArtifact, len(issues))
		for i, issue := range issues {
			identity, err := client.ArtifactIdentity(f.private.UID, issue.UID, store.InstanceUID())
			require.NoError(t, err)
			vector := []float32{1, float32(i + 1)}
			artifacts[i], err = embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{vector})
			require.NoError(t, err)
		}
		require.NotEqual(t, artifacts[0].Digest, artifacts[1].Digest)
		staleIndex, validIndex := 0, 1
		if artifacts[1].Digest < artifacts[0].Digest {
			staleIndex, validIndex = 1, 0
		}
		artifactStore := store.(db.EmbeddingArtifactStorage)
		for _, artifact := range artifacts {
			durable, err := artifactStore.RetainEmbeddingArtifact(ctx, artifact)
			require.NoError(t, err)
			require.True(t, durable)
		}
		staleIssue := issues[staleIndex]
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: staleIssue.ID, FromProjectID: f.private.ID, ToProjectID: destination.ID,
			IfMatchRev: staleIssue.Revision, Actor: "owner",
		})
		require.NoError(t, err)
		manifests, err := artifactStore.EmbeddingArtifactManifests(ctx, f.private.UID, 1)
		require.NoError(t, err)
		if assert.Len(t, manifests, 1) {
			assert.Equal(t, artifacts[validIndex].Digest, manifests[0].Digest,
				"a stale manifest must not consume the bounded status page")
		}
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: f.private.ID, Role: db.FederationRoleHub,
			HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true,
		})
		require.NoError(t, err)
		code, _, body := f.request(t, http.MethodGet,
			fmt.Sprintf("/api/v1/projects/%d/federation/status", f.private.ID), "member", nil, nil)
		assert.Equal(t, http.StatusOK, code, string(body))
		if code == http.StatusOK {
			var response struct {
				Statuses []struct {
					Embedding *struct {
						Artifacts []struct {
							Digest string `json:"digest"`
						} `json:"artifacts"`
					} `json:"embedding"`
				} `json:"statuses"`
			}
			require.NoError(t, json.Unmarshal(body, &response))
			require.Len(t, response.Statuses, 1)
			require.NotNil(t, response.Statuses[0].Embedding)
			assert.Len(t, response.Statuses[0].Embedding.Artifacts, 1)
			assert.Equal(t, artifacts[validIndex].Digest, response.Statuses[0].Embedding.Artifacts[0].Digest)
			assert.NotContains(t, string(body), artifacts[staleIndex].Digest)
		}
	})
}

// R7: a fingerprint match alone cannot establish the exact configured recipe.
func TestEmbeddingProducerStatusExactRecipe(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		var ix *vector.Index
		if pg, ok := store.(*pgstore.Store); ok {
			ix, err = vector.OpenPostgres(ctx, pg.DB)
		} else {
			ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
		}
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, ix.Close()) })
		var calls atomic.Int32
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(provider.Close)
		client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2})
		require.NoError(t, err)
		identity, err := client.ArtifactIdentity(f.private.UID, f.issue.UID, store.InstanceUID())
		require.NoError(t, err)
		identity.Model = "different-model"
		artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(f.issue.Title, f.issue.Body), [][]float32{{1, 0}})
		require.NoError(t, err)
		durable, err := store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
		require.NoError(t, err)
		require.True(t, durable)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, VectorIndex: ix, Embedder: client, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		f.server = httptest.NewServer(server.Handler())
		t.Cleanup(f.server.Close)
		code, _, body := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation/status", f.private.ID), "member", nil, nil)
		require.Equal(t, http.StatusOK, code, string(body))
		require.Contains(t, string(body), `"state":"incompatible"`)
		require.Contains(t, string(body), `"reason":"recipe_mismatch"`)
		require.NotContains(t, string(body), "vector_bytes")
		require.Zero(t, calls.Load())
	})
}

func embeddingStatusBackends(t *testing.T, check func(*testing.T, db.Storage)) {
	t.Helper()
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store db.Storage
			if backend == "sqlite" {
				native, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
				store = native
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, t.Context())
				t.Cleanup(cleanup)
				native, err := pgstore.Open(t.Context(), dsn)
				require.NoError(t, err)
				store = native
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			check(t, store)
		})
	}
}

// A retained artifact that fails validation against the current input is
// skipped by the worker. Status names it instead of reporting it as reusable.
func TestEmbeddingProducerStatusReportsRejectedArtifact(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		var ix *vector.Index
		if pg, ok := store.(*pgstore.Store); ok {
			ix, err = vector.OpenPostgres(ctx, pg.DB)
			require.NoError(t, err)
			release, err := ix.AcquireReconcilerLease(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, release()) })
		} else {
			ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
			require.NoError(t, err)
		}
		t.Cleanup(func() { require.NoError(t, ix.Close()) })
		client, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:1", Model: "example-model", Dims: 2})
		require.NoError(t, err)
		identity, err := client.ArtifactIdentity(f.private.UID, f.issue.UID, store.InstanceUID())
		require.NoError(t, err)
		claimed := embedding.EmbedText("Final title", "")
		artifact := mismatchedInputArtifact(t, identity, claimed, embedding.EmbedText("Other text", ""))
		durable, err := store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
		require.NoError(t, err)
		require.True(t, durable)
		title, body := "Final title", ""
		_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: f.issue.ID, Title: &title, Body: &body, Actor: "member"})
		require.NoError(t, err)
		key := client.Generation().Fingerprint()
		require.NoError(t, ix.EnsureBuilding(ctx, key, client.Generation()))
		_, err = ix.RefreshMirror(ctx, store)
		require.NoError(t, err)
		recipe, err := client.ArtifactIdentity("", "", store.InstanceUID())
		require.NoError(t, err)
		_, err = ix.ReconcileArtifacts(ctx, key, store, recipe)
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, VectorIndex: ix, Embedder: client, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		f.server = httptest.NewServer(server.Handler())
		t.Cleanup(f.server.Close)
		code, _, response := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation/status", f.private.ID), "member", nil, nil)
		require.Equal(t, http.StatusOK, code, string(response))
		require.Contains(t, string(response), `"state":"incompatible"`)
		require.Contains(t, string(response), `"reason":"invalid_artifact"`)
	})
}

// mismatchedInputArtifact builds an internally consistent artifact whose
// identity claims one input while its chunks describe another. The digest
// follows the documented complete-artifact construction.
func mismatchedInputArtifact(t *testing.T, identity embedding.ArtifactIdentity, claimed, actual string) embedding.EmbeddingArtifact {
	t.Helper()
	artifact, err := embedding.NewArtifact(identity, actual, [][]float32{{0, 1}})
	require.NoError(t, err)
	artifact.InputHash = embedding.ArtifactInputHash(claimed)
	manifest := artifact.Manifest()
	manifest.Digest = ""
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	h := sha256.New()
	_, _ = h.Write([]byte("kata.embedding-artifact.v1\x00"))
	sized := func(b []byte) {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(b)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(b)
	}
	sized(raw)
	for _, chunk := range artifact.Chunks {
		sized(chunk.VectorBytes)
	}
	artifact.Digest = hex.EncodeToString(h.Sum(nil))
	require.NoError(t, embedding.ValidateArtifact(artifact, "", ""))
	require.Error(t, embedding.ValidateArtifact(artifact, claimed, ""))
	return artifact
}

// A selected producer whose configured recipe changed cannot generate for the
// project until the root records the new recipe. Status names that state.
func TestEmbeddingProducerStatusReportsChangedLocalRecipe(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		selected, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:1", Model: "example-model-a", Dims: 2})
		require.NoError(t, err)
		recipe, err := selected.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: store.InstanceUID(), Recipe: recipe})
		require.NoError(t, err)
		_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: f.private.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
		require.NoError(t, err)
		configured, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:1", Model: "example-model-b", Dims: 2})
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Embedder: configured, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		f.server = httptest.NewServer(server.Handler())
		t.Cleanup(f.server.Close)
		path := fmt.Sprintf("/api/v1/projects/%d/federation/status", f.private.ID)
		code, _, response := f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, code, string(response))
		require.Contains(t, string(response), `"embedding":{"producer":`)
		require.Contains(t, string(response), `"reason":"producer_recipe_changed"`)

		matching := daemon.NewServer(daemon.ServerConfig{DB: store, Embedder: selected, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, matching.Close()) })
		f.server = httptest.NewServer(matching.Handler())
		t.Cleanup(f.server.Close)
		code, _, response = f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, code, string(response))
		require.NotContains(t, string(response), "producer_recipe_changed")
	})
}

// A selected producer whose upstream check failed reports the failure, so an
// operator can tell a broken producer from one that is simply waiting. The
// failure belongs to the selection it was observed under: after the root
// selects another producer, this daemon no longer reports it.
func TestEmbeddingProducerStatusReportsProducerFailure(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		client, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:1", Model: "example-model", Dims: 2})
		require.NoError(t, err)
		recipe, err := client.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		selected := db.ProjectEmbeddingProducer{ProducerInstanceUID: store.InstanceUID(), Recipe: recipe}
		selectProducer := func(producer db.ProjectEmbeddingProducer) {
			raw, err := json.Marshal(producer)
			require.NoError(t, err)
			_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: f.private.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
			require.NoError(t, err)
		}
		selectProducer(selected)
		message := "embedding producer unavailable: fetch hub project metadata: 503 Service Unavailable"
		health := daemon.ReconcilerHealth{Configured: true, ProducerErrors: map[string]daemon.ProducerFailure{f.private.UID: {Message: message, Producer: selected}}}
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Embedder: client, ReconcilerHealth: func() daemon.ReconcilerHealth { return health }, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		f.server = httptest.NewServer(server.Handler())
		t.Cleanup(f.server.Close)
		path := fmt.Sprintf("/api/v1/projects/%d/federation/status", f.private.ID)
		code, _, response := f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, code, string(response))
		require.Contains(t, string(response), `"reason":"producer_unavailable"`)
		require.Contains(t, string(response), `"last_error":"`+message+`"`)

		selectProducer(db.ProjectEmbeddingProducer{ProducerInstanceUID: "00000000000000000000000002", Recipe: recipe})
		code, _, response = f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, code, string(response))
		require.NotContains(t, string(response), "producer_unavailable", "another daemon is now the producer")
		require.NotContains(t, string(response), "last_error")

		selectProducer(selected)
		health.ProducerErrors = nil
		code, _, response = f.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, code, string(response))
		require.NotContains(t, string(response), "producer_unavailable")
	})
}

// A relay that upgraded to a new recipe version cannot read its selection's
// outdated recipe. Status still names the re-selection it needs.
func TestEmbeddingProducerStatusReportsOutdatedRecipeVersion(t *testing.T) {
	embeddingStatusBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		client, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:1", Model: "example-model", Dims: 2})
		require.NoError(t, err)
		recipe, err := client.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		path := fmt.Sprintf("/api/v1/projects/%d/federation/status", f.private.ID)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Embedder: client, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		f.server = httptest.NewServer(server.Handler())
		t.Cleanup(f.server.Close)
		for _, producerUID := range []string{store.InstanceUID(), "00000000000000000000000002"} {
			recipe.RecipeVersion = embedding.RecipeVersion - 1
			raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: producerUID, Recipe: recipe})
			require.NoError(t, err)
			blob, err := json.Marshal(map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw})
			require.NoError(t, err)
			// A selection written before an upgrade no longer validates.
			if pg, ok := store.(*pgstore.Store); ok {
				_, err = pg.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(blob), f.private.ID)
			} else {
				_, err = store.(*sqlitestore.Store).ExecContext(ctx, `UPDATE projects SET metadata=? WHERE id=?`, string(blob), f.private.ID)
			}
			require.NoError(t, err)
			code, _, response := f.request(t, http.MethodGet, path, "member", nil, nil)
			require.Equal(t, http.StatusOK, code, string(response))
			require.Contains(t, string(response), `"state":"rejected"`)
			if producerUID == store.InstanceUID() {
				require.Contains(t, string(response), `"reason":"producer_recipe_changed"`)
			} else {
				require.NotContains(t, string(response), "producer_recipe_changed", "only the selected producer can act on its recipe")
			}
		}
	})
}
