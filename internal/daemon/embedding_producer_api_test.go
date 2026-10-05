package daemon_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
)

// R7: an ordinary writable project credential cannot change producer policy;
// the daemon owner can. A caller's actor/producer labels are not authority.
func TestEmbeddingProducerMetadataRequiresOwner(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "producer-owner.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "producer-owner-project")
	require.NoError(t, err)
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "member-test-token", Actor: "member", AdminActor: "owner"})
	require.NoError(t, err)
	client, err := embedding.New(embedding.Config{BaseURL: "https://encoder.example", Model: "example-model", Dims: 2})
	require.NoError(t, err)
	recipe, err := client.ArtifactIdentity("", "", "")
	require.NoError(t, err)
	producer := db.ProjectEmbeddingProducer{ProducerInstanceUID: store.InstanceUID(), Recipe: recipe}
	server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "owner-test-token"}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	identityServer := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, identityServer.Close()) })
	probe := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/metadata", project.ID), bytes.NewBufferString(`{"actor":"member","patch":{"area":"example-area"}}`))
	probe.Header.Set("Content-Type", "application/json")
	probe.Header.Set("Authorization", "Bearer member-test-token")
	probeResponse := httptest.NewRecorder()
	identityServer.Handler().ServeHTTP(probeResponse, probe)
	require.Equal(t, http.StatusOK, probeResponse.Code, probeResponse.Body.String())
	for _, test := range []struct {
		name, token, actor string
		status             int
	}{
		{"ordinary_member", "member-test-token", "member", http.StatusForbidden},
		{"daemon_owner", "owner-test-token", "owner", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"actor": test.actor, "patch": map[string]any{db.ProjectEmbeddingMetadataKey: producer}})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/metadata", project.ID), bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			if test.name == "ordinary_member" {
				identityServer.Handler().ServeHTTP(response, req)
			} else {
				server.Handler().ServeHTTP(response, req)
			}
			require.Equal(t, test.status, response.Code, response.Body.String())
			current, err := store.ProjectByID(t.Context(), project.ID)
			require.NoError(t, err)
			configured, err := db.ProjectEmbeddingProducerFromMetadata(current.Metadata)
			require.NoError(t, err)
			if test.status == http.StatusForbidden {
				require.Nil(t, configured)
			} else {
				require.Equal(t, producer, *configured)
			}
		})
	}
}
