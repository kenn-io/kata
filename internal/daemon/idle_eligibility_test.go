package daemon_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestAutostartIdleShutdownRequiresOwnerLocalExposure(t *testing.T) {
	localDaemon := kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/kata.sock"}
	localWeb := daemon.WebEndpoint{
		Endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:27123"},
		Origin:   "http://127.0.0.1:27123",
	}

	tests := []struct {
		name     string
		daemon   kitdaemon.Endpoint
		web      daemon.WebEndpoint
		config   config.DaemonConfig
		eligible bool
	}{
		{name: "unix and loopback listeners", daemon: localDaemon, web: localWeb, eligible: true},
		{
			name:   "private network daemon listener",
			daemon: kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "100.64.0.5:7777"},
			web:    localWeb,
		},
		{
			name:   "externally published loopback listener",
			daemon: localDaemon,
			web: daemon.WebEndpoint{
				Endpoint: localWeb.Endpoint,
				Origin:   "https://daemon.example",
			},
		},
		{
			name:   "trusted proxy exposure",
			daemon: localDaemon,
			web:    localWeb,
			config: config.DaemonConfig{Auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Actor",
				TrustedProxyListeners: []string{"127.0.0.1:27123"},
			}}},
		},
		{
			name:   "shared listener with proxy host aliases",
			daemon: localDaemon,
			web: daemon.WebEndpoint{
				Endpoint: localWeb.Endpoint,
				Origin:   localWeb.Origin,
				Shared:   true,
			},
			config: config.DaemonConfig{Web: config.WebConfig{
				AllowedHosts: []string{"backend.example:7777"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.eligible, daemon.AutostartIdleShutdownEligible(tt.daemon, tt.web, tt.config))
		})
	}
}
