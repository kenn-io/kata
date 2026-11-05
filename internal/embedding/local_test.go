package embedding

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalEndpointPerBatch(t *testing.T) {
	first := newFakeServer(t, 200, `{"data":[{"embedding":[3,4]}]}`, "")
	second := newFakeServer(t, 200, `{"data":[{"embedding":[4,3]}]}`, "")
	defer first.Close()
	defer second.Close()
	calls := 0
	client, err := New(Config{Model: "nomic-embed-text", Dims: 2, Endpoint: func(context.Context) (string, error) {
		calls++
		if calls <= 2 {
			return first.URL, nil
		}
		return second.URL, nil
	}})
	require.NoError(t, err)
	require.Zero(t, calls)
	for range 2 {
		_, err = client.Embed(t.Context(), []string{"hello"})
		require.NoError(t, err)
	}
	require.Equal(t, 2, calls)
	first.Close()
	vectors, err := client.Embed(t.Context(), []string{"hello"})
	require.NoError(t, err)
	require.InDelta(t, 0.8, vectors[0][0], 0.00001)
	require.Equal(t, 3, calls)
}
