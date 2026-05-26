package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// DaemonConfig is the parsed contents of <KATA_HOME>/config.toml. The
// file is optional; an absent file yields a zero-value DaemonConfig and
// no error so callers can use this unconditionally at daemon start.
//
// Only daemon-side fields belong here. Client-side overrides
// (KATA_SERVER, .kata.local.toml) live in their own resolution path.
type DaemonConfig struct {
	// Listen is the bind address used by `kata daemon start` when no
	// --listen flag is supplied. Same syntax as the flag (host:port).
	// An empty value (or a missing file) means "default Unix socket".
	Listen string `toml:"listen"`
	// TUI carries client-side interactive UI defaults. Unlike remote
	// daemon overrides, these are user preferences and belong in
	// <KATA_HOME>/config.toml.
	TUI TUIConfig `toml:"tui"`
	// Close carries daemon-wide close-flow policy knobs.
	Close CloseConfig `toml:"close"`
	// Auth carries the daemon's bearer-auth token, if any.
	Auth AuthConfig `toml:"auth"`
}

// AuthConfig is the [auth] block of <KATA_HOME>/config.toml. An empty
// Token disables bearer auth — appropriate for Unix-socket and loopback-TCP
// deployments; non-loopback TCP requires either --insecure-readonly with no
// token, or a token plus TrustPrivateNetwork.
//
// KATA_AUTH_TOKEN, when set, overrides the TOML value. Use it for
// ephemeral or CI-only tokens that should never be persisted to disk.
// KATA_TRUST_PRIVATE_NETWORK=1 is equivalent to trust_private_network = true.
type AuthConfig struct {
	Token               string `toml:"token"`
	TrustPrivateNetwork bool   `toml:"trust_private_network"`
}

// TUIConfig holds TUI user preferences from <KATA_HOME>/config.toml.
type TUIConfig struct {
	// Mouse enables Bubble Tea mouse cell-motion capture and additive
	// click/wheel navigation. Default false preserves native selection.
	Mouse bool `toml:"mouse"`
}

// CloseConfig is the [close] block of <KATA_HOME>/config.toml.
type CloseConfig struct {
	Throttle CloseThrottleConfig `toml:"throttle"`
}

// CloseThrottleConfig toggles the sibling-burst and repeated-message
// guards. Enabled is a *bool so an absent key defaults to enabled —
// disabling is opt-in. Projects that rely on bulk-subagent close
// patterns can set `enabled = false` to skip the guards entirely.
//
// The on/off behavior is daemon-wide: every project served by this
// daemon picks up the same policy. Per-project knobs would need a
// project_settings table and are out of scope for v1.
type CloseThrottleConfig struct {
	Enabled *bool `toml:"enabled"`
}

// ThrottleEnabled returns the resolved policy: true when the key is
// absent or explicitly set to true, false only when explicitly disabled.
func (c CloseThrottleConfig) ThrottleEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ReadDaemonConfig parses <KATA_HOME>/config.toml. Returns a zero-value
// DaemonConfig and nil error when the file is absent — daemon startup
// should not fail just because the file isn't there. Other I/O or parse
// errors are returned so a typo doesn't silently fall back to defaults.
func ReadDaemonConfig() (*DaemonConfig, error) {
	path, err := DaemonConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from KATA_HOME, not user input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			var cfg DaemonConfig
			applyDaemonConfigEnv(&cfg)
			return &cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg DaemonConfig
	meta, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if u := meta.Undecoded(); len(u) > 0 {
		keys := make([]string, len(u))
		for i, k := range u {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("parse %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	cfg.Listen = strings.TrimSpace(cfg.Listen)
	cfg.Auth.Token = strings.TrimSpace(cfg.Auth.Token)
	applyDaemonConfigEnv(&cfg)
	return &cfg, nil
}

func applyDaemonConfigEnv(cfg *DaemonConfig) {
	if v := strings.TrimSpace(os.Getenv("KATA_AUTH_TOKEN")); v != "" {
		cfg.Auth.Token = v
	}
	if EnvTruthy("KATA_TRUST_PRIVATE_NETWORK") {
		cfg.Auth.TrustPrivateNetwork = true
	}
}

// EnvTruthy reports whether an environment variable is set to a recognized
// true value for kata config overlays.
func EnvTruthy(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v == "1" || strings.EqualFold(v, "true")
}
