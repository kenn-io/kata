package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigureOriginPinnedRedirectsKeepsStandardRedirectLimit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/another-hop", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	httpClient := server.Client()
	require.NoError(t, ConfigureOriginPinnedRedirects(httpClient, server.URL))
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		server.URL+"/start",
		nil,
	)
	require.NoError(t, err)

	response, err := httpClient.Do(request)
	require.Error(t, err)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	assert.Contains(t, err.Error(), "stopped after 10 redirects")
	assert.Equal(t, int32(10), requests.Load())
}
