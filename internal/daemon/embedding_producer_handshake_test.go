package daemon_test

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// R7: the existing authorized enrollment and metadata handshake advertise the
// exact stable producer/recipe without granting inventory after revocation.
func TestEmbeddingProducerHandshake(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		public, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		client, err := embedding.New(embedding.Config{BaseURL: "https://encoder.example", Model: "example-model", Dims: 2})
		require.NoError(t, err)
		recipe, err := client.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		producer := db.ProjectEmbeddingProducer{ProducerInstanceUID: store.InstanceUID(), Recipe: recipe}
		raw, err := json.Marshal(producer)
		require.NoError(t, err)
		_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: f.private.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
		require.NoError(t, err)
		body := map[string]any{"spoke_instance_uid": "00000000000000000000000006", "project_id": f.private.ID, "actor": "member", "capabilities": "pull,push,claim", "token": "producer-handshake-test-token", "relay": map[string]any{"protocol_version": 1, "serve_downstream": true}}
		status, _, response := f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
		require.Equal(t, http.StatusOK, status, string(response))
		verify := func(data []byte) {
			var envelope struct {
				Relay map[string]jsontext.Value `json:"relay"`
			}
			require.NoError(t, json.Unmarshal(data, &envelope))
			require.Contains(t, envelope.Relay, "embedding_producer")
			require.JSONEq(t, string(raw), string(envelope.Relay["embedding_producer"]))
		}
		verify(response)
		path := fmt.Sprintf("/api/v1/projects/%d/federation/metadata", f.private.ID)
		headers := map[string]string{"Authorization": "Bearer producer-handshake-test-token"}
		status, _, response = f.request(t, http.MethodGet, path, "", nil, headers)
		require.Equal(t, http.StatusOK, status, string(response))
		verify(response)
		parent, err := store.ResolveAPIToken(ctx, "member-test-token")
		require.NoError(t, err)
		_, _, err = store.RevokeAPIToken(ctx, parent.ID, "owner")
		require.NoError(t, err)
		status, _, response = f.request(t, http.MethodGet, path, "", nil, headers)
		require.Equal(t, http.StatusForbidden, status, string(response))
		require.NotContains(t, string(response), "example-model")
		require.NotContains(t, string(response), store.InstanceUID())
	})
}
