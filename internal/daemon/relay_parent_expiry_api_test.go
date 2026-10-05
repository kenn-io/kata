package daemon_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R3: ordinary parent credentials may expire without gaining subtree scope.
func TestRelayParentExpirationTokenAPI(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		before := time.Now().UTC()
		status, _, body := f.request(t, http.MethodPost, "/api/v1/tokens", "", map[string]any{"actor": "relay-member", "expires_in_seconds": 3600}, map[string]string{"Authorization": "Bearer bootstrap-test-token"})
		require.Equal(t, http.StatusOK, status, string(body))
		var out struct {
			Token     db.APIToken `json:"token"`
			Plaintext string      `json:"plaintext"`
		}
		require.NoError(t, json.Unmarshal(body, &out))
		require.NotEmpty(t, out.Plaintext)
		retained, err := store.ResolveAPIToken(t.Context(), out.Plaintext)
		require.NoError(t, err)
		require.Nil(t, retained.Scope)
		require.NotNil(t, retained.ExpiresAt)
		require.WithinDuration(t, before.Add(time.Hour), *retained.ExpiresAt, 2*time.Second)
		for _, seconds := range []int64{-1, 9223372036854775807} {
			status, _, body = f.request(t, http.MethodPost, "/api/v1/tokens", "", map[string]any{"actor": "relay-member", "expires_in_seconds": seconds}, map[string]string{"Authorization": "Bearer bootstrap-test-token"})
			require.Equal(t, http.StatusBadRequest, status, string(body))
		}
	})
}
