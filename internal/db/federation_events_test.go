package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"hegel.dev/go/hegel"
)

func TestProjectMetadataSpokeAdoptionPayloadOmitsRootProducer(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		fieldCount := hegel.Draw(ht, hegel.Integers(0, 8))
		producerValue := jsontext.Value(`{"producer_instance_uid":"hub","recipe":{}}`)
		metadata := map[string]jsontext.Value{
			db.ProjectEmbeddingMetadataKey: producerValue,
			"nested":                       jsontext.Value(`{"federation_embedding":"ordinary"}`),
		}
		want := make(map[string]jsontext.Value, fieldCount)
		want["nested"] = metadata["nested"]
		for i := range fieldCount {
			key := fmt.Sprintf("field-%d", i)
			value, err := json.Marshal(hegel.Draw(ht, hegel.Integers(-1000, 1000)))
			require.NoError(ht, err)
			metadata[key] = jsontext.Value(value)
			want[key] = jsontext.Value(value)
		}
		metadataRaw, err := json.Marshal(metadata)
		require.NoError(ht, err)
		payload, err := db.ProjectMetadataSpokeAdoptionPayload(db.JSONBlob(metadataRaw))
		require.NoError(ht, err)

		var result struct {
			Diff map[string]struct {
				From jsontext.Value `json:"from"`
				To   jsontext.Value `json:"to"`
			} `json:"diff"`
		}
		require.NoError(ht, json.Unmarshal([]byte(payload), &result))
		require.NotContains(ht, result.Diff, db.ProjectEmbeddingMetadataKey)
		require.Len(ht, result.Diff, len(want))
		for key, value := range want {
			entry, ok := result.Diff[key]
			require.True(ht, ok, "ordinary metadata field %q is retained", key)
			require.Equal(ht, "null", string(entry.From))
			require.Equal(ht, value, entry.To)
		}

		genericPayload, err := db.ProjectMetadataAdoptionPayload(db.JSONBlob(metadataRaw))
		require.NoError(ht, err)
		var genericResult struct {
			Diff map[string]struct {
				From jsontext.Value `json:"from"`
				To   jsontext.Value `json:"to"`
			} `json:"diff"`
		}
		require.NoError(ht, json.Unmarshal([]byte(genericPayload), &genericResult))
		require.Len(ht, genericResult.Diff, len(metadata), "generic baselines preserve all top-level metadata")
		for key, value := range metadata {
			entry, ok := genericResult.Diff[key]
			require.True(ht, ok, "generic baseline retains metadata field %q", key)
			require.Equal(ht, "null", string(entry.From))
			require.Equal(ht, value, entry.To)
		}
		require.Equal(ht, producerValue, genericResult.Diff[db.ProjectEmbeddingMetadataKey].To,
			"generic hub and relay baselines retain the root-owned producer")
	}, hegel.WithTestCases(1000))
}
