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
	_, _, err := runCmdCapture(t, nil, "daemon", "start", "--foreground")
	require.ErrorContains(t, err, "web.public_origin")
}

func TestTUIConfigIgnoresInvalidServerConfig(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[tui]\nmouse = true\n"+untrustedWebConfig), 0600))
	got, err := resolveTUIConfig(newTUICmd(), false)
	require.NoError(t, err)
	assert.True(t, got.Mouse)
}

func TestFederationSpokeAuthIgnoresInvalidServerConfig(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprintf("named=%v", named), func(t *testing.T) {
			home := setupKataEnv(t)
			resetFlags(t)
			t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
			require.NoError(t, os.Unsetenv("KATA_TRUST_PRIVATE_NETWORK"))
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

func TestDoctorDaemonFallbackIgnoresInvalidServerConfig(t *testing.T) {
	home := setupKataEnv(t)
	resetFlags(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	require.NoError(t, os.Unsetenv("KATA_TRUST_PRIVATE_NETWORK"))
	t.Setenv("KATA_SERVER", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("active_daemon = \"spoke\"\n[[daemon]]\nname = \"spoke\"\nurl = \"https://daemon.example\"\ntoken = \"spoke-token\"\n"+untrustedWebConfig), 0600))
	state := doctorState{workspaceValid: false}
	resolved, err := state.resolveDoctorDaemon(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "https://daemon.example", resolved.BaseURL)
	assert.Equal(t, "spoke-token", resolved.Token)
}

func TestFederationEmbeddingRecipeIgnoresInvalidServerConfig(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	require.NoError(t, os.Unsetenv("KATA_TRUST_PRIVATE_NETWORK"))
	data := untrustedWebConfig + "[search.embeddings]\nbase_url = \"https://embedding.example/v1\"\nmodel = \"example-model\"\ndims = 2\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(data), 0600))
	output, _, err := runCmdCapture(t, nil, "federation", "embedding-recipe")
	require.NoError(t, err)
	assert.Contains(t, output, `"model":"example-model"`)
}
