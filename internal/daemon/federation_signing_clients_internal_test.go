package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
)

func TestNativeLeaseAndRebindClientsSignEveryAttempt(t *testing.T) {
	t.Setenv("TEST_NATIVE_SIGNING_KEY", strings.Repeat("k", 64))
	s := signingpolicy.Source{KeyID: "key-a", KeyEnv: "TEST_NATIVE_SIGNING_KEY"}
	inputs := map[string]bool{}
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.Header.Get("Signature"))
		require.Equal(t, "Bearer enrollment", r.Header.Get("Authorization"))
		input := r.Header.Get("Signature-Input")
		require.False(t, inputs[input])
		inputs[input] = true
		_, _ = w.Write([]byte(`{"project_id":1,"project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX"}`))
	}))
	defer hub.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = hub.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	c, err := newClaimHubClient(t.Context(), hub.URL, "enrollment", false, &s)
	require.NoError(t, err)
	for range 2 {
		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, hub.URL, nil)
		require.NoError(t, reqErr)
		r, err := c.client.Do(req)
		require.NoError(t, err)
		_ = r.Body.Close()
	}
	_, err = fetchFederationRebindMetadata(t.Context(), hub.URL, "enrollment", 1, &s)
	require.NoError(t, err)
	require.Len(t, inputs, 3)
}
