package daemon_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A11: the current account and credential expiry are available to ordinary
// clients through their existing authority reads, without token inventory.
func TestActiveConnectionAccountAndExpiry(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: "connected-account", AdminActor: "admin", PlaintextToken: "connected-account-test-token", ExpiresAt: &expiry})
		require.NoError(t, err)
		for _, path := range []string{"/api/v1/instance", "/api/v1/ui/snapshot?view=all"} {
			t.Run(path, func(t *testing.T) {
				code, _, body := f.request(t, http.MethodGet, path, "connected-account", nil, nil)
				require.Equal(t, http.StatusOK, code, string(body))
				var response struct {
					Auth struct {
						Actor     string     `json:"actor"`
						ExpiresAt *time.Time `json:"expires_at"`
					} `json:"auth"`
					Capabilities struct {
						Account        string     `json:"account"`
						ExpiresAt      *time.Time `json:"expires_at"`
						TokenAuditRead bool       `json:"token_audit_read"`
					} `json:"capabilities"`
				}
				require.NoError(t, json.Unmarshal(body, &response))
				if path == "/api/v1/instance" {
					require.Equal(t, "connected-account", response.Auth.Actor)
					require.Equal(t, &expiry, response.Auth.ExpiresAt, "unscoped expiring credentials must retain current expiry")
				} else {
					require.Equal(t, "connected-account", response.Capabilities.Account)
					require.Equal(t, &expiry, response.Capabilities.ExpiresAt)
					require.False(t, response.Capabilities.TokenAuditRead, "showing the active account must not enable credential inventory")
				}
				require.NotContains(t, string(body), "connected-account-test-token")
			})
		}
	})
}
