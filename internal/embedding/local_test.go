package embedding

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalEndpointPerCall(t *testing.T) {
	first := newFakeServer(t, 200, `{"data":[{"embedding":[3,4]}]}`, "")
	second := newFakeServer(t, 200, `{"data":[{"embedding":[4,3]}]}`, "")
	defer first.Close()
	defer second.Close()
	endpointErr := errors.New("local server unavailable")
	calls := 0
	client, err := New(Config{BaseURL: "", Model: "m", Dims: 2, Endpoint: func(context.Context) (string, error) {
		calls++
		if calls == 3 {
			return "", endpointErr
		}
		if calls == 1 {
			return first.URL, nil
		}
		return second.URL, nil
	}})
	require.NoError(t, err)
	require.Zero(t, calls)
	_, err = client.Embed(t.Context(), []string{"hello"})
	require.NoError(t, err)
	vectors, err := client.Embed(t.Context(), []string{"hello"})
	require.NoError(t, err)
	require.InDelta(t, 0.8, vectors[0][0], 0.00001)
	require.Equal(t, 2, calls)
	_, err = client.Embed(t.Context(), []string{"hello"})
	require.ErrorIs(t, err, endpointErr)
	require.Equal(t, endpointErr.Error(), client.CredentialHealth().LastError)
}
