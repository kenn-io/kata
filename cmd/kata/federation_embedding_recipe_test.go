package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// R7/R9: owners can export an exact preferred recipe from local configuration
// without document generation, daemon connectivity, or credential disclosure.
func TestFederationEmbeddingRecipeExport(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(provider.Close)
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_SERVER", "https://unreachable.example")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[search.embeddings]\nbase_url = \""+provider.URL+"\"\nmodel = \"example-model\"\ndims = 2\napi_key = \"recipe-test-secret-never-export\"\n"), 0600))
	var output bytes.Buffer
	cmd := newFederationCmd()
	cmd.SilenceUsage = true
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"embedding-recipe"})
	require.NoError(t, cmd.Execute())
	require.True(t, bytes.HasSuffix(output.Bytes(), []byte("\n")), "recipe JSON must terminate its text line")
	var recipe embedding.ArtifactIdentity
	require.NoError(t, json.Unmarshal(output.Bytes(), &recipe))
	require.Equal(t, "example-model", recipe.Model)
	require.Equal(t, 2, recipe.Dimensions)
	require.Equal(t, 2, recipe.RecipeVersion)
	require.Equal(t, 2000, recipe.SplitMaxRunes)
	require.Equal(t, 200, recipe.SplitOverlap)
	require.Equal(t, embedding.ArtifactFloat32Encoding, recipe.Encoding)
	require.Len(t, recipe.RecipeFingerprint, 64)
	require.Empty(t, recipe.ProjectUID)
	require.Empty(t, recipe.IssueUID)
	require.Empty(t, recipe.InputHash)
	require.Empty(t, recipe.ProducerInstanceUID)
	require.NotContains(t, output.String(), "recipe-test-secret-never-export")
	require.NotContains(t, output.String(), provider.URL)
	require.Zero(t, calls.Load())
	// The documented workflow embeds the exact stdout as the reserved recipe.
	// Validate with the actual strict metadata consumer, including nested keys.
	metadata, err := json.Marshal(map[string]any{db.ProjectEmbeddingMetadataKey: map[string]any{
		"producer_instance_uid": "00000000000000000000000002",
		"recipe":                jsontext.Value(output.Bytes()),
	}})
	require.NoError(t, err)
	producer, err := db.ProjectEmbeddingProducerFromMetadata(db.JSONBlob(metadata))
	require.NoError(t, err)
	require.Equal(t, recipe, producer.Recipe)
}

func TestFederationEmbeddingRecipeUnconfigured(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	var output bytes.Buffer
	cmd := newFederationCmd()
	cmd.SilenceUsage = true
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"embedding-recipe"})
	err := cmd.Execute()
	require.ErrorContains(t, err, "embeddings are not configured")
	require.Empty(t, output.String())
}
