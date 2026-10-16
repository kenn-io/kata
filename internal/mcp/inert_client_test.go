package mcpserver

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

// Catalog-only tests still use an initialized client so production can bind
// immutable request limits. Any accidental daemon request fails locally.
func inertTestClient(t *testing.T) *kataclient.Client {
	t.Helper()
	client, err := kataclient.NewWithHTTPClient("http://daemon.invalid", &http.Client{Transport: inertMCPTransport{}})
	require.NoError(t, err)
	return client
}

type inertMCPTransport struct{}

func (inertMCPTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected daemon request in catalog-only test")
}
