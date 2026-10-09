package daemon_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/pkg/client/generated"
)

// The daemon serializes this domain value directly in handshake/status. The
// generated SDK must validate the emitted recipe without document identities.
func TestEmbeddingProducerRecipeGeneratedContract(t *testing.T) {
	client, err := embedding.New(embedding.Config{BaseURL: "https://embedding.example", Model: "example-model", Dims: 2})
	require.NoError(t, err)
	recipe, err := client.ArtifactIdentity("", "", "")
	require.NoError(t, err)
	producer := db.ProjectEmbeddingProducer{ProducerInstanceUID: "00000000000000000000000001", Recipe: recipe}
	require.NoError(t, producer.Validate())
	raw, err := json.Marshal(producer)
	require.NoError(t, err)
	var wire generated.ProjectEmbeddingProducer
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.NoError(t, wire.Recipe.Validate(), "generated validator must accept the daemon's exact recipe")
	require.NoError(t, wire.Validate())
}
