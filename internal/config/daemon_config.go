package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DaemonConfig is the parsed contents of <KATA_HOME>/config.toml. The
// file is optional; an absent file yields a zero-value DaemonConfig and
// no error so callers can use this unconditionally at daemon start.
//
// This file holds daemon-side settings plus user-level client defaults such as
// TUI preferences and the named daemon catalog. Workspace-local remote
// overrides (KATA_SERVER, .kata.local.toml) live in their own resolution path.
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
	// Storage carries DB selection plus backend-specific startup policy; see
	// config.KataDSN for DSN precedence (env > file > default).
	Storage StorageConfig `toml:"storage"`
	// Search carries opt-in semantic-search settings. An empty
	// [search.embeddings] section leaves kata on lexical-only FTS.
	Search SearchConfig `toml:"search"`
	// GitHubSync carries daemon-owned GitHub API credential settings for
	// background synchronization.
	GitHubSync GitHubSyncConfig `toml:"github_sync"`
}

// SearchConfig is the [search] block of <KATA_HOME>/config.toml. Today it only
// nests [search.embeddings]; lexical FTS needs no configuration.
type SearchConfig struct {
	Embeddings EmbeddingsConfig `toml:"embeddings"`
}

// EmbeddingsConfig is the [search.embeddings] block. It is opt-in: when BaseURL
// and Model are both empty kata stays on lexical-only search. BaseURL and Model
// must be set together (a partial config is rejected at load), and APIKey is
// mutually exclusive with APIKeyEnv, mirroring the [[daemon]] token pattern.
type EmbeddingsConfig struct {
	// BaseURL is the OpenAI-compatible endpoint base, e.g.
	// "http://localhost:11434/v1". The client appends "/embeddings".
	BaseURL string `toml:"base_url"`
	// Model is the embedding model name sent in each request.
	Model string `toml:"model"`
	// APIKey is the inline bearer token, mutually exclusive with APIKeyEnv.
	APIKey string `toml:"api_key"`
	// APIKeyEnv names an environment variable holding the bearer token, so
	// the secret stays out of the config file.
	APIKeyEnv string `toml:"api_key_env"`
	// FingerprintSalt is the operator's lever for "same model name,
	// different weights"; changing it invalidates every stored vector.
	FingerprintSalt string `toml:"fingerprint_salt"`
	// Dims is the expected vector dimensionality. 0 means use the client
	// default. Negative values are rejected.
	Dims int `toml:"dims"`
	// BatchSize caps inputs per embedding request. 0 means the client
	// default; negative values are rejected.
	BatchSize int `toml:"batch_size"`
	// TimeoutSeconds is the per-request HTTP timeout. 0 means the client
	// default; negative values are rejected.
	TimeoutSeconds int `toml:"timeout_seconds"`
	// TrustPrivateNetwork allows a plaintext bearer token to a private-network
	// endpoint, mirroring [auth].trust_private_network.
	TrustPrivateNetwork bool `toml:"trust_private_network"`
}

// Enabled reports whether semantic search is configured. Both base_url and
// model are required; validateEmbeddings rejects a partial config so Enabled
// never sees a half-set section.
func (e EmbeddingsConfig) Enabled() bool {
	return strings.TrimSpace(e.BaseURL) != "" && strings.TrimSpace(e.Model) != ""
}

// ResolvedAPIKey returns the literal api_key, or the value of the environment
// variable named by api_key_env. Returns "" when neither is set.
func (e EmbeddingsConfig) ResolvedAPIKey() string {
	if e.APIKey != "" {
		return e.APIKey
	}
	if e.APIKeyEnv != "" {
		return os.Getenv(strings.TrimSpace(e.APIKeyEnv))
	}
	return ""
}

// StorageConfig is the [storage] block of <KATA_HOME>/config.toml. An empty
// DSN means "no override from the file" — env (KATA_DSN, KATA_DB) or the
// default <KATA_HOME>/kata.db wins. See config.KataDSN.
type StorageConfig struct {
	DSN      string                `toml:"dsn"`
	Postgres PostgresStorageConfig `toml:"postgres"`
}

// PostgresStorageConfig controls the schema selected by Postgres connections
// and whether a serving process may prepare it. Empty fields select pgstore's
// standalone defaults: schema "kata" and mode "bootstrap".
type PostgresStorageConfig struct {
	Schema        string `toml:"schema"`
	Mode          string `toml:"mode"`
	SchemaOwner   string `toml:"schema_owner"`
	AllowInsecure bool   `toml:"allow_insecure"`
}

// DefaultGitHubSyncTokenEnv is the environment variable name used for
// daemon GitHub sync tokens when [github_sync].token_env is omitted.
const DefaultGitHubSyncTokenEnv = "KATA_GITHUB_TOKEN" // #nosec G101 -- environment variable name, not a credential value.

// DefaultGitHubSyncTokenHost is the GitHub host that may receive the default
// daemon GitHub sync token.
const DefaultGitHubSyncTokenHost = "github.com"

// GitHubSyncConfig is the [github_sync] block of <KATA_HOME>/config.toml.
type GitHubSyncConfig struct {
	TokenEnv  string            `toml:"token_env"`
	TokenHost string            `toml:"token_host"`
	Apps      []GitHubAppConfig `toml:"app"`
}

// TokenEnvName returns the configured GitHub sync token environment variable,
// defaulting to KATA_GITHUB_TOKEN when unset.
func (c GitHubSyncConfig) TokenEnvName() string {
	if v := strings.TrimSpace(c.TokenEnv); v != "" {
		return v
	}
	return DefaultGitHubSyncTokenEnv
}

// TokenHostName returns the GitHub host allowed to receive the configured env
// token, defaulting to github.com when unset.
func (c GitHubSyncConfig) TokenHostName() string {
	if v := strings.ToLower(strings.TrimSpace(c.TokenHost)); v != "" {
		return v
	}
	return DefaultGitHubSyncTokenHost
}

// GitHubAppConfig is one [[github_sync.app]] credential entry.
type GitHubAppConfig struct {
	Host           string `toml:"host"`
	Owner          string `toml:"owner"`
	AppID          int64  `toml:"app_id"`
	InstallationID int64  `toml:"installation_id"`
	PrivateKeyPath string `toml:"private_key_path"`
}

// AuthConfig is the [auth] block of <KATA_HOME>/config.toml. An empty
// Token disables bearer auth — appropriate for Unix-socket and loopback-TCP
// deployments; non-loopback TCP requires either --insecure-readonly with no
// token, a token plus TrustPrivateNetwork, or the explicit
// AllowUnauthenticatedPrivateNetworkWrites opt-in on a literal private IP bind.
//
// KATA_AUTH_TOKEN, when set, overrides the TOML value. Use it for
// ephemeral or CI-only tokens that should never be persisted to disk.
// KATA_TRUST_PRIVATE_NETWORK=1 is equivalent to trust_private_network = true.
// KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES=1 is equivalent to
// allow_unauthenticated_private_network_writes = true.
type AuthConfig struct {
	Token                                    string      `toml:"token"`
	TrustPrivateNetwork                      bool        `toml:"trust_private_network"`
	AllowUnauthenticatedPrivateNetworkWrites bool        `toml:"allow_unauthenticated_private_network_writes"`
	RequireTokenIdentity                     bool        `toml:"require_token_identity"`
	Proxy                                    ProxyConfig `toml:"proxy"`
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

const defaultCloseThrottleWindow = 60 * time.Second

// CloseThrottleConfig toggles the opt-in sibling-burst and repeated-message
// guards. Enabled is a *bool so an absent key defaults to disabled; operators
// who want burst throttling can set `enabled = true`.
//
// The on/off behavior is daemon-wide: every project served by this
// daemon picks up the same policy. Per-project knobs would need a
// project_settings table and are out of scope for v1.
type CloseThrottleConfig struct {
	Enabled *bool  `toml:"enabled"`
	Window  string `toml:"window"`
}

// ThrottleEnabled returns the resolved sibling-burst policy. Burst throttling
// is off unless explicitly enabled.
func (c CloseThrottleConfig) ThrottleEnabled() bool {
	if c.Enabled == nil {
		return false
	}
	return *c.Enabled
}

// ThrottleWindow returns the resolved sibling-burst look-back window. The
// default is 60 seconds; configured values use Go duration syntax, such as
// "30s", "2m", or "1h".
func (c CloseThrottleConfig) ThrottleWindow() (time.Duration, error) {
	if strings.TrimSpace(c.Window) == "" {
		return defaultCloseThrottleWindow, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(c.Window))
	if err != nil {
		return 0, fmt.Errorf("close.throttle.window: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("close.throttle.window must be positive")
	}
	return d, nil
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
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME, not user input
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
		cfg.Storage.Postgres.Schema = strings.TrimSpace(cfg.Storage.Postgres.Schema)
		cfg.Storage.Postgres.Mode = strings.TrimSpace(cfg.Storage.Postgres.Mode)
		cfg.Close.Throttle.Window = strings.TrimSpace(cfg.Close.Throttle.Window)
		trimSearchEmbeddings(&cfg)
		trimDaemonCatalog(&cfg)
		trimGitHubSync(&cfg)
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
	if _, err := cfg.Close.Throttle.ThrottleWindow(); err != nil {
		return nil, err
	}
	if err := validateEmbeddings(cfg.Search.Embeddings); err != nil {
		return nil, err
	}
	if err := validateGitHubSync(cfg.GitHubSync); err != nil {
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
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME, not user input
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

func trimSearchEmbeddings(cfg *DaemonConfig) {
	e := &cfg.Search.Embeddings
	e.BaseURL = strings.TrimSpace(e.BaseURL)
	e.Model = strings.TrimSpace(e.Model)
	e.APIKey = strings.TrimSpace(e.APIKey)
	e.APIKeyEnv = strings.TrimSpace(e.APIKeyEnv)
	e.FingerprintSalt = strings.TrimSpace(e.FingerprintSalt)
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

func trimGitHubSync(cfg *DaemonConfig) {
	cfg.GitHubSync.TokenEnv = strings.TrimSpace(cfg.GitHubSync.TokenEnv)
	cfg.GitHubSync.TokenHost = strings.ToLower(strings.TrimSpace(cfg.GitHubSync.TokenHost))
	for i := range cfg.GitHubSync.Apps {
		app := &cfg.GitHubSync.Apps[i]
		app.Host = strings.ToLower(strings.TrimSpace(app.Host))
		if app.Host == "" {
			app.Host = "github.com"
		}
		app.Owner = strings.TrimSpace(app.Owner)
		app.PrivateKeyPath = strings.TrimSpace(app.PrivateKeyPath)
	}
}

func validateGitHubSync(cfg GitHubSyncConfig) error {
	seen := make(map[string]struct{}, len(cfg.Apps))
	for i := range cfg.Apps {
		app := cfg.Apps[i]
		if app.Owner == "" {
			return errors.New("github_sync.app: owner is required")
		}
		if app.AppID <= 0 {
			return errors.New("github_sync.app: app_id is required")
		}
		if app.InstallationID <= 0 {
			return errors.New("github_sync.app: installation_id is required")
		}
		if app.PrivateKeyPath == "" {
			return errors.New("github_sync.app: private_key_path is required")
		}
		key := app.Host + "/" + strings.ToLower(app.Owner)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("github_sync.app: duplicate app for %s", key)
		}
		seen[key] = struct{}{}
	}
	return nil
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

// validateEmbeddings rejects partial or contradictory [search.embeddings]
// config. base_url and model must both be set or both omitted (a half-set
// section is an operator mistake, not a usable default). api_key and
// api_key_env are mutually exclusive, mirroring the [[daemon]] token rule.
// The numeric knobs are sizes/durations, so negatives are invalid.
func validateEmbeddings(e EmbeddingsConfig) error {
	hasBase := strings.TrimSpace(e.BaseURL) != ""
	hasModel := strings.TrimSpace(e.Model) != ""
	if hasBase != hasModel {
		return errors.New(
			"search.embeddings: base_url and model must both be set or both omitted")
	}
	if e.APIKey != "" && e.APIKeyEnv != "" {
		return errors.New(
			"search.embeddings: api_key and api_key_env are mutually exclusive")
	}
	if e.Dims < 0 || e.BatchSize < 0 || e.TimeoutSeconds < 0 {
		return errors.New(
			"search.embeddings: dims, batch_size, and timeout_seconds must be non-negative")
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
	if EnvTruthy("KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES") {
		cfg.Auth.AllowUnauthenticatedPrivateNetworkWrites = true
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
	applyPostgresStorageEnv(&cfg.Storage.Postgres)
}

func applyPostgresStorageEnv(cfg *PostgresStorageConfig) {
	if v := strings.TrimSpace(os.Getenv("KATA_POSTGRES_SCHEMA")); v != "" {
		cfg.Schema = v
	}
	if v := strings.TrimSpace(os.Getenv("KATA_POSTGRES_SCHEMA_MODE")); v != "" {
		cfg.Mode = v
	}
	if v := os.Getenv("KATA_POSTGRES_SCHEMA_OWNER"); v != "" {
		cfg.SchemaOwner = v
	}
	if EnvTruthy("KATA_POSTGRES_ALLOW_INSECURE") {
		cfg.AllowInsecure = true
	}
}

// EnvTruthy reports whether an environment variable is set to a recognized
// true value for kata config overlays.
func EnvTruthy(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v == "1" || strings.EqualFold(v, "true")
}
