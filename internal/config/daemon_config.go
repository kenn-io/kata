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
	// An empty value (or a missing file) means the platform default:
	// Unix socket on Unix platforms, loopback TCP on Windows.
	Listen string `toml:"listen"`
	// ActiveDaemon names the daemon catalog entry selected by default.
	// Empty preserves the legacy implicit endpoint resolution.
	ActiveDaemon string `toml:"active_daemon"`
	// Daemons is the named daemon catalog. The TUI resolves its default
	// target here; other clients may also read this shared catalog, so it is
	// top-level rather than nested under [tui].
	Daemons []CatalogDaemonConfig `toml:"daemon"`
	// TUI carries client-side interactive UI defaults. Unlike remote
	// daemon overrides, these are user preferences and belong in
	// <KATA_HOME>/config.toml.
	TUI TUIConfig `toml:"tui"`
	// Close carries daemon-wide close-flow policy knobs.
	Close CloseConfig `toml:"close"`
	// Auth carries the daemon's bearer-auth token, if any.
	Auth AuthConfig `toml:"auth"`
	// Storage carries DB-selection settings. Today only `dsn` is honored;
	// see config.KataDSN for the full precedence (env > file > default).
	Storage StorageConfig `toml:"storage"`
}

// StorageConfig is the [storage] block of <KATA_HOME>/config.toml. An empty
// DSN means "no override from the file" — env (KATA_DSN, KATA_DB) or the
// default <KATA_HOME>/kata.db wins. See config.KataDSN.
type StorageConfig struct {
	DSN string `toml:"dsn"`
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
	Token                string      `toml:"token"`
	TrustPrivateNetwork  bool        `toml:"trust_private_network"`
	RequireTokenIdentity bool        `toml:"require_token_identity"`
	Proxy                ProxyConfig `toml:"proxy"`
}

// ProxyConfig is the [auth.proxy] sub-table. Both keys empty/absent means
// trusted-proxy actor mode is off; this is the default.
type ProxyConfig struct {
	TrustedActorHeader    string   `toml:"trusted_actor_header"`
	TrustedProxyListeners []string `toml:"trusted_proxy_listeners"`
}

// TUIConfig holds TUI user preferences from <KATA_HOME>/config.toml.
type TUIConfig struct {
	// Mouse enables Bubble Tea mouse cell-motion capture and additive
	// click/wheel navigation. Default false preserves native selection.
	Mouse bool `toml:"mouse"`
}

// CatalogDaemonConfig is a single named entry in the daemon catalog
// (top-level [[daemon]] in <KATA_HOME>/config.toml).
type CatalogDaemonConfig struct {
	Name  string `toml:"name"`
	Local bool   `toml:"local"`
	URL   string `toml:"url"`
	// Token is the inline bearer token, mutually exclusive with TokenEnv.
	Token string `toml:"token"`
	// TokenEnv names an environment variable holding the bearer token, so
	// the secret stays out of the config file. Resolved by clients only when
	// they select this daemon target.
	TokenEnv      string `toml:"token_env"`
	AllowInsecure bool   `toml:"allow_insecure"`
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
	var cfg DaemonConfig
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from KATA_HOME, not user input
	switch {
	case err == nil:
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
		cfg.Auth.Proxy.TrustedActorHeader = strings.TrimSpace(cfg.Auth.Proxy.TrustedActorHeader)
		cfg.Storage.DSN = strings.TrimSpace(cfg.Storage.DSN)
		trimDaemonCatalog(&cfg)
	case errors.Is(err, os.ErrNotExist):
		// Absent file: fall through with zero-value cfg. Env merge and
		// validation below still apply so an env-only misconfig is
		// caught the same way a TOML-only one is.
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	applyDaemonConfigEnv(&cfg)
	if err := validateAuthProxy(cfg.Auth.Proxy); err != nil {
		return nil, err
	}
	if err := normalizeDaemonCatalog(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ReadAuthConfig parses only the daemon auth settings from <KATA_HOME>/config.toml.
// It intentionally skips daemon-catalog normalization so auth-only clients do not
// lose [auth] settings because an unrelated catalog entry is unavailable.
func ReadAuthConfig() (AuthConfig, error) {
	path, err := DaemonConfigPath()
	if err != nil {
		return AuthConfig{}, err
	}
	var cfg DaemonConfig
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from KATA_HOME, not user input
	switch {
	case err == nil:
		meta, err := toml.Decode(string(data), &cfg)
		if err != nil {
			return AuthConfig{}, fmt.Errorf("parse %s: %w", path, err)
		}
		if u := meta.Undecoded(); len(u) > 0 {
			keys := make([]string, len(u))
			for i, k := range u {
				keys[i] = k.String()
			}
			return AuthConfig{}, fmt.Errorf("parse %s: unknown key(s): %s", path, strings.Join(keys, ", "))
		}
		cfg.Auth.Token = strings.TrimSpace(cfg.Auth.Token)
		cfg.Auth.Proxy.TrustedActorHeader = strings.TrimSpace(cfg.Auth.Proxy.TrustedActorHeader)
	case errors.Is(err, os.ErrNotExist):
		// Absent file: env overlays below still apply.
	default:
		return AuthConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	applyDaemonConfigEnv(&cfg)
	if err := validateAuthProxy(cfg.Auth.Proxy); err != nil {
		return AuthConfig{}, err
	}
	return cfg.Auth, nil
}

func trimDaemonCatalog(cfg *DaemonConfig) {
	cfg.ActiveDaemon = strings.TrimSpace(cfg.ActiveDaemon)
	for i := range cfg.Daemons {
		cfg.Daemons[i].Name = strings.TrimSpace(cfg.Daemons[i].Name)
		cfg.Daemons[i].URL = strings.TrimSpace(cfg.Daemons[i].URL)
		cfg.Daemons[i].Token = strings.TrimSpace(cfg.Daemons[i].Token)
		cfg.Daemons[i].TokenEnv = strings.TrimSpace(cfg.Daemons[i].TokenEnv)
	}
}

func normalizeDaemonCatalog(cfg *DaemonConfig) error {
	names := make(map[string]struct{}, len(cfg.Daemons))
	for i := range cfg.Daemons {
		d := &cfg.Daemons[i]
		if d.Name == "" {
			return errors.New("daemon: name is required")
		}
		if _, ok := names[d.Name]; ok {
			return fmt.Errorf("daemon: duplicate daemon name %q", d.Name)
		}
		names[d.Name] = struct{}{}
		if d.Local == (d.URL != "") {
			return fmt.Errorf("daemon %q: exactly one of local or url is required", d.Name)
		}
		if d.Token != "" && d.TokenEnv != "" {
			return fmt.Errorf("daemon %q: token and token_env are mutually exclusive", d.Name)
		}
	}
	if cfg.ActiveDaemon != "" {
		if _, ok := names[cfg.ActiveDaemon]; !ok {
			return fmt.Errorf("active_daemon %q is not in daemon catalog", cfg.ActiveDaemon)
		}
	}
	return nil
}

// validateAuthProxy rejects the dangerous partial-config case where the
// operator names a trusted-proxy header but forgets to enumerate any trusted
// listeners. A silent no-op there would look like proxy attribution is enabled
// while the daemon still trusts whatever body actor a client supplied.
//
// The inverse (listeners without a header) is dead config — the mode stays off
// because the header name is empty — and is accepted silently.
func validateAuthProxy(p ProxyConfig) error {
	if p.TrustedActorHeader != "" && len(p.TrustedProxyListeners) == 0 {
		return errors.New(
			"auth.proxy: trusted_actor_header is set but trusted_proxy_listeners is empty. " +
				"Set both to enable proxy attribution, or unset the header to disable")
	}
	return nil
}

func applyDaemonConfigEnv(cfg *DaemonConfig) {
	if v := strings.TrimSpace(os.Getenv("KATA_AUTH_TOKEN")); v != "" &&
		(!cfg.Auth.RequireTokenIdentity || !EnvTruthy("KATA_AUTOSTART")) {
		cfg.Auth.Token = v
	}
	if EnvTruthy("KATA_TRUST_PRIVATE_NETWORK") {
		cfg.Auth.TrustPrivateNetwork = true
	}
	if v := strings.TrimSpace(os.Getenv("KATA_TRUSTED_ACTOR_HEADER")); v != "" {
		cfg.Auth.Proxy.TrustedActorHeader = v
	}
	if raw := os.Getenv("KATA_TRUSTED_PROXY_LISTENERS"); raw != "" {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				out = append(out, t)
			}
		}
		cfg.Auth.Proxy.TrustedProxyListeners = out
	}
}

// EnvTruthy reports whether an environment variable is set to a recognized
// true value for kata config overlays.
func EnvTruthy(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v == "1" || strings.EqualFold(v, "true")
}
