package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const untrustedWebConfig = "[web]\npublic_origin = \"http://daemon.example\"\n"

func TestDaemonStartRejectsInvalidServerConfig(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(untrustedWebConfig), 0600))
	_, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.ErrorContains(t, err, "web.public_origin")
}

func TestFederationSpokeAuthIgnoresInvalidServerConfig(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprintf("named=%v", named), func(t *testing.T) {
			home := setupKataEnv(t)
			resetFlags(t)
			t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
			t.Setenv("KATA_SERVER", "")
			t.Setenv("KATA_AUTH_TOKEN", "unrelated-local-token")
			t.Chdir(t.TempDir())
			spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/ping" {
					_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"test"}`)
					return
				}
				assert.Equal(t, "Bearer spoke-token", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(spoke.Close)
			data := fmt.Sprintf("active_daemon = \"spoke\"\n[[daemon]]\nname = \"spoke\"\nurl = %q\ntoken = \"spoke-token\"\n", spoke.URL) + untrustedWebConfig
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(data), 0600))
			if named {
				flags.Daemon = "spoke"
			}
			hc, err := federationSpokeHTTPClient(t.Context(), spoke.URL)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, spoke.URL+"/probe", nil)
			require.NoError(t, err)
			response, err := hc.Do(req)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			assert.Equal(t, http.StatusNoContent, response.StatusCode)
		})
	}
}
