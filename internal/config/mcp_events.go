package config

import (
	"fmt"
	"strings"
	"time"
)

// DefaultMCPEventsMaxTTL caps finite MCP Events subscription grants when
// [mcp_events].max_ttl is unset.
const DefaultMCPEventsMaxTTL = 30 * 24 * time.Hour

// MinMCPEventsMaxTTL is the shortest grant the protocol hands out, so a cap
// below it would contradict the grant floor.
const MinMCPEventsMaxTTL = 60 * time.Second

// MCPEventsConfig is the [mcp_events] block. Events is off unless enabled.
type MCPEventsConfig struct {
	// Enabled starts the daemon-owned MCP Events delivery service.
	Enabled bool `toml:"enabled"`
	// MaxTTL caps finite subscription grants as a Go duration such as "720h".
	// Empty selects 30 days.
	MaxTTL string `toml:"max_ttl"`
	// AllowNoExpiry honors subscriber requests for a grant that never
	// expires. Unset allows them; false gives such requests a max_ttl grant.
	AllowNoExpiry *bool `toml:"allow_no_expiry"`
}

// NoExpiryAllowed reports whether no-expiry grant requests are honored.
func (c MCPEventsConfig) NoExpiryAllowed() bool {
	return c.AllowNoExpiry == nil || *c.AllowNoExpiry
}

// MaxTTLDuration parses the grant cap.
func (c MCPEventsConfig) MaxTTLDuration() (time.Duration, error) {
	raw := strings.TrimSpace(c.MaxTTL)
	if raw == "" {
		return DefaultMCPEventsMaxTTL, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < MinMCPEventsMaxTTL {
		return 0, fmt.Errorf("mcp_events.max_ttl must be a duration of at least %s: %q", MinMCPEventsMaxTTL, c.MaxTTL)
	}
	return d, nil
}
