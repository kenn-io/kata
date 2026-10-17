package daemon

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"net/http/httptest"
	"testing"
)

func TestMCPCapabilities(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := ServerConfig{Auth: config.AuthConfig{Token: "daemon-token"}}
		if enabled {
			cfg.MCP = testMCPService{}
		}
		s := NewServer(cfg)
		r := httptest.NewRequest("GET", "/api/v1/mcp/capabilities", nil)
		r.Header.Set("Authorization", "Bearer daemon-token")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		require.Equal(t, 200, w.Code, w.Body.String())
		var out struct {
			Enabled  bool   `json:"enabled"`
			Endpoint string `json:"endpoint"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		require.Equal(t, enabled, out.Enabled)
		if enabled {
			require.Equal(t, "/mcp", out.Endpoint)
		} else {
			require.Empty(t, out.Endpoint)
		}
		require.NoError(t, s.Close())
	}
}
