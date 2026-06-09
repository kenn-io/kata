package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
)

// remoteServerEnvVar is the environment variable that names a kata
// daemon URL. When set, it takes precedence over .kata.local.toml.
const remoteServerEnvVar = "KATA_SERVER"

// allowInsecureEnvVar opts out of the plain-http guard for KATA_SERVER.
// Truthy values: "1", "true". Has no effect on .kata.local.toml; the
// equivalent there is `[server].allow_insecure = true`.
const allowInsecureEnvVar = "KATA_ALLOW_INSECURE"

type activeRemoteTarget struct {
	Name          string
	BaseURL       string
	Token         string
	TokenEnv      string
	AllowInsecure bool
}

// ErrRemoteUnavailable wraps probe failures against an explicitly
// configured remote URL. Callers translate
// this into a daemon-unavailable CLI error; we keep the package free
// of CLI-layer types so this package stays importable from the TUI.
var ErrRemoteUnavailable = errors.New("kata server not responding")

// ResolveRemote is the exported view of resolveRemote so callers
// outside client (e.g. cmd/kata health) can honor the same
// KATA_SERVER / .kata.local.toml / active_daemon resolution rules without
// auto-starting a local daemon.
func ResolveRemote(ctx context.Context, workspaceStart string) (string, bool, error) {
	return resolveRemote(ctx, workspaceStart)
}

// NormalizeRemoteURL exposes kata's remote URL validation/canonicalization
// for TUI daemon-catalog entries. It returns scheme://host[:port] with path
// and query stripped, and applies the same allow_insecure semantics used by
// KATA_SERVER, .kata.local.toml, and active daemon-catalog entries.
func NormalizeRemoteURL(v string, allowInsecure bool) (string, error) {
	return normalizeRemoteURL(v, allowInsecure)
}

// RemoteAllowInsecureForBaseURL reports whether the configured remote source
// for workspaceStart explicitly opted baseURL into plaintext HTTP.
func RemoteAllowInsecureForBaseURL(baseURL, workspaceStart string) bool {
	return remoteAllowInsecureForBaseURL(baseURL, workspaceStart)
}

// resolveRemote checks the opt-in remote sources, in order:
//
//  1. KATA_SERVER env (highest precedence)
//  2. .kata.local.toml [server].url walked up from workspaceStart
//     (CWD when workspaceStart is empty)
//  3. active_daemon in <KATA_HOME>/config.toml when it names a remote
//     catalog entry
//
// If neither is set, returns ("", false, nil) and the caller falls
// through to local Discover/auto-start. If a URL is configured, the
// helper probes /api/v1/ping; on success it returns (url, true, nil),
// on failure it returns ("", false, ErrRemoteUnavailable wrapped with
// the URL and the source name) so the user sees which input is wrong.
//
// workspaceStart lets callers that target a specific workspace (via
// `--workspace`) anchor the .kata.local.toml walk there instead of
// CWD; otherwise running from outside the repo with `--workspace`
// would silently miss the workspace's local override.
func resolveRemote(ctx context.Context, workspaceStart string) (string, bool, error) {
	if v := os.Getenv(remoteServerEnvVar); v != "" {
		u, err := normalizeRemoteURL(v, envAllowInsecure())
		if err != nil {
			return "", false, fmt.Errorf("KATA_SERVER %q: %w", v, err)
		}
		if !probeRemote(ctx, u) {
			return "", false, fmt.Errorf("%w: %s (KATA_SERVER)", ErrRemoteUnavailable, u)
		}
		return u, true, nil
	}
	root, path, ok := findLocalConfig(workspaceStart)
	if !ok {
		return resolveActiveRemote(ctx)
	}
	cfg, err := config.ReadLocalConfig(root)
	if err != nil {
		if errors.Is(err, config.ErrLocalConfigMissing) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	if cfg.Server.URL == "" {
		return resolveActiveRemote(ctx)
	}
	u, err := normalizeRemoteURL(cfg.Server.URL, cfg.Server.AllowInsecure)
	if err != nil {
		return "", false, fmt.Errorf("%s server.url %q: %w", path, cfg.Server.URL, err)
	}
	if !probeRemote(ctx, u) {
		return "", false, fmt.Errorf("%w: %s (%s)", ErrRemoteUnavailable, u, path)
	}
	return u, true, nil
}

func resolveActiveRemote(ctx context.Context) (string, bool, error) {
	target, ok, err := activeRemoteFromConfig()
	if err != nil || !ok {
		return "", false, err
	}
	if !globalAuthTokenOverrideSet() {
		_, err = resolveActiveRemoteTargetToken(target)
	}
	if err != nil {
		return "", false, err
	}
	if !probeRemote(ctx, target.BaseURL) {
		return "", false, fmt.Errorf("%w: %s (%s active_daemon %q)",
			ErrRemoteUnavailable, target.BaseURL, daemonConfigSource(), target.Name)
	}
	return target.BaseURL, true, nil
}

func activeRemoteFromConfig() (activeRemoteTarget, bool, error) {
	cfg, err := config.ReadDaemonConfig()
	if err != nil {
		return activeRemoteTarget{}, false, err
	}
	if cfg.ActiveDaemon == "" {
		return activeRemoteTarget{}, false, nil
	}
	for _, daemon := range cfg.Daemons {
		if daemon.Name != cfg.ActiveDaemon {
			continue
		}
		if daemon.Local {
			return activeRemoteTarget{}, false, nil
		}
		baseURL, err := normalizeRemoteURL(daemon.URL, daemon.AllowInsecure)
		if err != nil {
			return activeRemoteTarget{}, false,
				fmt.Errorf("%s daemon %q url %q: %w",
					daemonConfigSource(), daemon.Name, daemon.URL, err)
		}
		return activeRemoteTarget{
			Name:          daemon.Name,
			BaseURL:       baseURL,
			Token:         daemon.Token,
			TokenEnv:      daemon.TokenEnv,
			AllowInsecure: daemon.AllowInsecure,
		}, true, nil
	}
	return activeRemoteTarget{}, false, nil
}

func resolveActiveRemoteTargetToken(target activeRemoteTarget) (string, error) {
	if target.TokenEnv == "" {
		return target.Token, nil
	}
	token := strings.TrimSpace(os.Getenv(target.TokenEnv))
	if token == "" {
		return "", fmt.Errorf("daemon %q: token_env %q is unset or empty",
			target.Name, target.TokenEnv)
	}
	return token, nil
}

func activeRemoteTargetAuthForBaseURL(baseURL, workspaceStart string) (TargetAuth, bool, error) {
	if globalAuthTokenOverrideSet() || higherPriorityRemoteSourceMatchesBaseURL(baseURL, workspaceStart) {
		return TargetAuth{}, false, nil
	}
	target, ok, err := activeRemoteFromConfig()
	if err != nil || !ok {
		return TargetAuth{}, false, err
	}
	if target.BaseURL != strings.TrimRight(baseURL, "/") {
		return TargetAuth{}, false, nil
	}
	token, err := resolveActiveRemoteTargetToken(target)
	if err != nil {
		return TargetAuth{}, false, err
	}
	return TargetAuth{
		Token:         token,
		AllowInsecure: target.AllowInsecure,
	}, true, nil
}

func globalAuthTokenOverrideSet() bool {
	return strings.TrimSpace(os.Getenv("KATA_AUTH_TOKEN")) != ""
}

func higherPriorityRemoteSourceMatchesBaseURL(baseURL, workspaceStart string) bool {
	baseURL = strings.TrimRight(baseURL, "/")
	if v := os.Getenv(remoteServerEnvVar); v != "" {
		u, err := normalizeRemoteURL(v, envAllowInsecure())
		return err == nil && u == baseURL
	}
	root, _, ok := findLocalConfig(workspaceStart)
	if !ok {
		return false
	}
	cfg, err := config.ReadLocalConfig(root)
	if err != nil || cfg == nil || cfg.Server.URL == "" {
		return false
	}
	u, err := normalizeRemoteURL(cfg.Server.URL, cfg.Server.AllowInsecure)
	return err == nil && u == baseURL
}

func activeRemoteAllowInsecureForBaseURL(baseURL string) bool {
	target, ok, err := activeRemoteFromConfig()
	return err == nil && ok && target.BaseURL == baseURL && target.AllowInsecure
}

func daemonConfigSource() string {
	path, err := config.DaemonConfigPath()
	if err != nil {
		return "<KATA_HOME>/config.toml"
	}
	return path
}

// envAllowInsecure reports whether KATA_ALLOW_INSECURE is set to a
// truthy value. Accepts "1" and "true" (case-insensitive) with
// surrounding whitespace trimmed.
func envAllowInsecure() bool {
	v := strings.TrimSpace(os.Getenv(allowInsecureEnvVar))
	return v == "1" || strings.EqualFold(v, "true")
}

func remoteAllowInsecureForBaseURL(baseURL, workspaceStart string) bool {
	if v := os.Getenv(remoteServerEnvVar); v != "" {
		allow := envAllowInsecure()
		u, err := normalizeRemoteURL(v, allow)
		return err == nil && u == baseURL && allow
	}
	root, _, ok := findLocalConfig(workspaceStart)
	if !ok {
		return activeRemoteAllowInsecureForBaseURL(baseURL)
	}
	cfg, err := config.ReadLocalConfig(root)
	if err != nil {
		return false
	}
	if cfg.Server.URL != "" {
		if !cfg.Server.AllowInsecure {
			return false
		}
		u, err := normalizeRemoteURL(cfg.Server.URL, true)
		return err == nil && u == baseURL
	}
	return activeRemoteAllowInsecureForBaseURL(baseURL)
}

// findLocalConfig walks upward from start looking for .kata.local.toml,
// but caps the walk at the closest workspace boundary (an ancestor
// directory containing .kata.toml or .git). The file is only honored
// when it sits at or below that boundary.
//
// Without the boundary check, an attacker on a multi-user system
// could plant a .kata.local.toml in a shared ancestor like /tmp; a
// victim running kata from a descendant would walk up to the shared
// dir and route their daemon traffic to the attacker's URL. Anchoring
// to a workspace boundary closes that hole and matches the documented
// contract — .kata.local.toml lives next to .kata.toml.
//
// When start is empty the walk begins at CWD. Commands targeting a
// specific workspace via --workspace pass that path so the walk
// honors the targeted workspace rather than wherever the user
// happens to be.
func findLocalConfig(start string) (root, path string, ok bool) {
	dir := start
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", "", false
		}
	}

	// Track the closest .kata.local.toml we see; only return it after
	// we confirm we hit a workspace boundary at or above its level.
	var localRoot, localPath string
	foundLocal := false

	for {
		if !foundLocal {
			candidate := filepath.Join(dir, config.LocalConfigFilename)
			if _, err := os.Stat(candidate); err == nil {
				localRoot, localPath, foundLocal = dir, candidate, true
			} else if !errors.Is(err, os.ErrNotExist) {
				// Permission denied, broken symlink, etc. — surface to
				// stderr so the user is not silently routed past their
				// config file.
				fmt.Fprintf(os.Stderr, "kata: warning: cannot stat %s: %v\n", candidate, err)
			}
		}
		if isWorkspaceBoundary(dir) {
			if foundLocal {
				return localRoot, localPath, true
			}
			return "", "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Walked to filesystem root without ever crossing a
			// workspace boundary. A .kata.local.toml found in a
			// shared ancestor without a workspace anchor is
			// unverifiable provenance — drop it.
			return "", "", false
		}
		dir = parent
	}
}

// isWorkspaceBoundary reports whether dir holds a marker that anchors
// a kata workspace: either a committed .kata.toml binding, or a .git
// directory/file (covering both regular repos and worktrees). Either
// is enough — pre-init flows in a freshly cloned repo can drop a
// .kata.local.toml beside .git before .kata.toml exists.
func isWorkspaceBoundary(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, config.ProjectConfigFilename)); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	return false
}

// normalizeRemoteURL parses a value as an http(s) URL and returns the
// canonical scheme://host[:port] form (no path, no query). Empty path
// matches the daemon's expectation: callers append /api/v1/... themselves.
//
// Enforces a scheme guard: plain http is allowed only for private IP
// literals (loopback, RFC1918, CGNAT, link-local, ULA — the same set
// the daemon's listen-address validator accepts). Hostnames over plain
// http and public IPs over plain http are rejected unless allowInsecure
// is true. https URLs are unaffected.
func normalizeRemoteURL(v string, allowInsecure bool) (string, error) {
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("url must include host")
	}
	if err := requireSecureOrPrivate(u, allowInsecure); err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

// requireSecureOrPrivate returns nil when the URL is safe to dial over
// the given network posture. https is always safe; plain http is only
// safe for a private IP literal. allowInsecure short-circuits the check
// for users who know what they're doing (e.g. talking to an internal
// service over a hostname inside a private overlay).
func requireSecureOrPrivate(u *url.URL, allowInsecure bool) error {
	if u.Scheme == "https" || allowInsecure {
		return nil
	}
	host := u.Hostname()
	if err := daemon.ValidateNonPublicAddress(net.JoinHostPort(host, "1")); err != nil {
		return fmt.Errorf("plain http to %q is not allowed: %w; use https or set allow_insecure (env KATA_ALLOW_INSECURE=1, or [server].allow_insecure=true in .kata.local.toml)", host, err)
	}
	return nil
}

// probeRemote does a 1-second /api/v1/ping check against base. We keep
// the budget tight: a misconfigured remote should fail fast, not stall
// the user behind the 5-second auto-start deadline.
//
// Indirected through a package-level var so tests that exercise
// resolution paths past the probe (e.g. allow_insecure bypass) can
// stub the network call without dialing TEST-NET addresses.
var probeRemote = func(ctx context.Context, base string) bool {
	client := &http.Client{Timeout: 1 * time.Second}
	return Ping(ctx, client, base)
}
