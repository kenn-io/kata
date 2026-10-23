package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

func TestMCPEventsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		enabled     bool
		maxTTL      time.Duration
		wantInvalid bool
	}{
		{name: "absent is disabled with the 30 day cap", maxTTL: 30 * 24 * time.Hour},
		{name: "enabled alone", body: "[mcp_events]\nenabled = true\n", enabled: true, maxTTL: 30 * 24 * time.Hour},
		{name: "explicit cap", body: "[mcp_events]\nenabled = true\nmax_ttl = \"48h\"\n", enabled: true, maxTTL: 48 * time.Hour},
		{name: "the minimum grant is the smallest cap", body: "[mcp_events]\nmax_ttl = \"60s\"\n", maxTTL: time.Minute},
		{name: "cap below the minimum grant", body: "[mcp_events]\nmax_ttl = \"30s\"\n", wantInvalid: true},
		{name: "unparsable cap", body: "[mcp_events]\nmax_ttl = \"forever\"\n", wantInvalid: true},
		{name: "negative cap", body: "[mcp_events]\nmax_ttl = \"-1h\"\n", wantInvalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.body), 0o600))
			cfg, err := config.ReadDaemonConfig()
			if tc.wantInvalid {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "mcp_events.max_ttl")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.enabled, cfg.MCPEvents.Enabled)
			maxTTL, err := cfg.MCPEvents.MaxTTLDuration()
			require.NoError(t, err)
			assert.Equal(t, tc.maxTTL, maxTTL)
		})
	}
}
