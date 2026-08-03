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
	gitcmd "go.kenn.io/kit/git/cmd"
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

// ErrNamedDaemonNotFound marks a --daemon/catalog selection that does not
// match any [[daemon]] entry in <KATA_HOME>/config.toml.
var ErrNamedDaemonNotFound = errors.New("named daemon not found")

type namedDaemonTarget struct {
	Name          string
	Local         bool
	BaseURL       string
	Token         string
	AllowInsecure bool
}

// ResolveRemote is the exported view of resolveRemote so callers
// outside client (e.g. cmd/kata health) can honor the same
// KATA_SERVER / .kata.local.toml / active_daemon resolution rules without
// auto-starting a local daemon.
func ResolveRemote(ctx context.Context, workspaceStart string) (string, bool, error) {
	return resolveRemote(ctx, workspaceStart)
}

// DiscoverNamed returns the base URL for a named daemon catalog entry without
// starting a local daemon. Local entries inspect runtime files only; remote
// entries are normalized and probed.
func DiscoverNamed(ctx context.Context, name string) (string, bool, error) {
	target, ok, err := discoverNamedDaemonTarget(ctx, name)
	if err != nil || !ok {
		return "", ok, err
	}
	return target.BaseURL, true, nil
}

// EnsureNamedRunning returns the base URL for a named daemon catalog entry,
// auto-starting local entries and probing remote entries. It is an explicit
// per-invocation selection and therefore ignores KATA_SERVER, .kata.local.toml,
// and active_daemon.
func EnsureNamedRunning(ctx context.Context, name string) (string, error) {
	target, err := resolveNamedDaemonTarget(ctx, name)
	if err != nil {
		return "", err
	}
	return target.BaseURL, nil
}

// NormalizeRemoteURL exposes kata's remote URL validation/canonicalization
// for TUI daemon-catalog entries. It returns scheme://host[:port] with path
// and query stripped, and applies the same allow_insecure semantics used by
// KATA_SERVER, .kata.local.toml, and active daemon-catalog entries.
func NormalizeRemoteURL(v string, allowInsecure bool) (string, error) {
	return normalizeRemoteURL(v, allowInsecure)
}

// NormalizeRemoteBaseURL applies remote transport policy while preserving a
// configured reverse-proxy path prefix.
func NormalizeRemoteBaseURL(v string, allowInsecure bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("url must include host")
	}
	if err := requireSecureOrPrivate(u, allowInsecure); err != nil {
		return "", err
	}
	return config.CanonicalHTTPBaseURL(u.String())
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
	root, path, ok, err := findLocalConfig(workspaceStart)
	if err != nil {
		return "", false, err
	}
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

func discoverNamedDaemonTarget(ctx context.Context, name string) (namedDaemonTarget, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return namedDaemonTarget{}, false, fmt.Errorf("%w: empty name", ErrNamedDaemonNotFound)
	}
	cfg, err := config.ReadDaemonConfig()
	if err != nil {
		return namedDaemonTarget{}, false, err
	}
	for _, d := range cfg.Daemons {
		if d.Name != name {
			continue
		}
		if d.Local {
			ns, err := daemon.NewNamespace()
			if err != nil {
				return namedDaemonTarget{}, false, err
			}
			baseURL, ok := Discover(ctx, ns.DataDir)
			if !ok {
				return namedDaemonTarget{}, false, nil
			}
			target, err := namedDaemonTargetFromCatalog(d, baseURL, true)
			if err != nil {
				return namedDaemonTarget{}, false, err
			}
			return target, true, nil
		}
		target, err := namedDaemonTargetFromCatalog(d, "", false)
		if err != nil {
			return namedDaemonTarget{}, false, err
		}
		if !probeRemote(ctx, target.BaseURL) {
			return namedDaemonTarget{}, false, fmt.Errorf("%w: %s (%s daemon %q)",
				ErrRemoteUnavailable, target.BaseURL, daemonConfigSource(), d.Name)
		}
		return target, true, nil
	}
	return namedDaemonTarget{}, false, fmt.Errorf("%w: %q", ErrNamedDaemonNotFound, name)
}

func resolveNamedDaemonTarget(ctx context.Context, name string) (namedDaemonTarget, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return namedDaemonTarget{}, fmt.Errorf("%w: empty name", ErrNamedDaemonNotFound)
	}
	cfg, err := config.ReadDaemonConfig()
	if err != nil {
		return namedDaemonTarget{}, err
	}
	for _, d := range cfg.Daemons {
		if d.Name != name {
			continue
		}
		if d.Local {
			baseURL, err := EnsureLocalRunning(ctx)
			if err != nil {
				return namedDaemonTarget{}, err
			}
			return namedDaemonTargetFromCatalog(d, baseURL, true)
		}
		target, err := namedDaemonTargetFromCatalog(d, "", false)
		if err != nil {
			return namedDaemonTarget{}, err
		}
		if !probeRemote(ctx, target.BaseURL) {
			return namedDaemonTarget{}, fmt.Errorf("%w: %s (%s daemon %q)",
				ErrRemoteUnavailable, target.BaseURL, daemonConfigSource(), d.Name)
		}
		return target, nil
	}
	return namedDaemonTarget{}, fmt.Errorf("%w: %q", ErrNamedDaemonNotFound, name)
}

func namedDaemonTargetForBaseURL(name, baseURL string) (namedDaemonTarget, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return namedDaemonTarget{}, fmt.Errorf("%w: empty name", ErrNamedDaemonNotFound)
	}
	cfg, err := config.ReadDaemonConfig()
	if err != nil {
		return namedDaemonTarget{}, err
	}
	for _, d := range cfg.Daemons {
		if d.Name != name {
			continue
		}
		if d.Local {
			return namedDaemonTargetFromCatalog(d, strings.TrimRight(baseURL, "/"), true)
		}
		return namedDaemonTargetFromCatalog(d, "", false)
	}
	return namedDaemonTarget{}, fmt.Errorf("%w: %q", ErrNamedDaemonNotFound, name)
}

func namedDaemonTargetFromCatalog(
	daemon config.CatalogDaemonConfig,
	localBaseURL string,
	local bool,
) (namedDaemonTarget, error) {
	baseURL := localBaseURL
	if !local {
		var err error
		baseURL, err = normalizeRemoteURL(daemon.URL, daemon.AllowInsecure)
		if err != nil {
			return namedDaemonTarget{}, fmt.Errorf("%s daemon %q url %q: %w",
				daemonConfigSource(), daemon.Name, daemon.URL, err)
		}
	}
	target := activeRemoteTarget{
		Name:          daemon.Name,
		BaseURL:       baseURL,
		Token:         daemon.Token,
		TokenEnv:      daemon.TokenEnv,
		AllowInsecure: daemon.AllowInsecure,
	}
	token := daemon.Token
	if local || !globalAuthTokenOverrideSet() {
		var err error
		token, err = resolveActiveRemoteTargetToken(target)
		if err != nil {
			return namedDaemonTarget{}, err
		}
	}
	return namedDaemonTarget{
		Name:          daemon.Name,
		Local:         local,
		BaseURL:       baseURL,
		Token:         token,
		AllowInsecure: daemon.AllowInsecure,
	}, nil
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
	auth := resolveAuthConfig()
	return TargetAuth{
		Token:               token,
		AllowInsecure:       target.AllowInsecure,
		TrustPrivateNetwork: auth.TrustPrivateNetwork,
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
	// An unverifiable local config (findErr != nil) aborts resolution in
	// resolveRemote, so no client reaches this point through it; report
	// no match rather than guessing at its contents.
	root, _, ok, findErr := findLocalConfig(workspaceStart)
	if findErr != nil || !ok {
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
	root, _, ok, findErr := findLocalConfig(workspaceStart)
	if findErr != nil {
		// Unverifiable provenance never grants a plaintext downgrade.
		return false
	}
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
//
// A .kata.local.toml discovered at the boundary is additionally
// refused when it is git-tracked in its containing repo (see
// localConfigTracked): the file is meant to be per-developer and
// gitignored, so a committed one has unverifiable provenance and
// could redirect [server].url to route a victim's bearer token to an
// attacker-controlled host.
//
// A non-nil error means a .kata.local.toml was found but its tracked
// status could not be verified inside a git worktree. That case must
// abort resolution rather than fall through: the file may be a
// developer's legitimate override, and silently ignoring it on a
// transient git failure would reroute a state-changing command to an
// active or auto-started local daemon. (A determined-tracked file, by
// contrast, is definitively illegitimate, so treating it as absent
// restores exactly the pre-attack behavior.)
func findLocalConfig(start string) (root, path string, ok bool, err error) {
	dir := start
	if dir == "" {
		dir, err = os.Getwd()
		if err != nil {
			return "", "", false, nil
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
				tracked, determined := localConfigTrackState(localRoot)
				switch {
				case determined && tracked:
					// A .kata.local.toml that is committed into the repo has
					// attacker-controlled provenance: a hostile contributor
					// can `git add -f` it past the gitignore and point
					// [server].url at a host they control, so a victim who
					// runs kata in the checkout (with a global bearer token
					// configured) misroutes that token. The file is meant to
					// be per-developer and gitignored; a tracked one is never
					// honored as a server/URL override. Drop it and fall
					// through to active_daemon/local resolution.
					fmt.Fprintf(os.Stderr, "kata: warning: ignoring %s: it is tracked by git; a committed .kata.local.toml is not honored as a server override (keep it untracked/gitignored)\n", localPath)
					return "", "", false, nil
				case !determined && gitWorktreePresent(localRoot):
					// Tracked status could not be established (git missing,
					// broken, dubious-ownership, or the query timed out) but we
					// ARE inside a git worktree, where the file could be a
					// committed redirect. Fail closed with a hard error: an
					// attacker who can make the git query fail must not get an
					// unverifiable override honored, and a legitimate override
					// must not be silently dropped in favor of another daemon.
					return "", "", false, fmt.Errorf(
						"cannot verify the git-tracked status of %s inside a git repository; "+
							"refusing it as a server override — ensure git is available and the "+
							"checkout is trusted, or remove the file", localPath)
				default:
					// Either determined-untracked (legitimate gitignored
					// developer override) or unknown with no .git present at
					// all (a genuine non-repo workspace anchored only by
					// .kata.toml, where there is no notion of tracking). Honor.
					return localRoot, localPath, true, nil
				}
			}
			return "", "", false, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Walked to filesystem root without ever crossing a
			// workspace boundary. A .kata.local.toml found in a
			// shared ancestor without a workspace anchor is
			// unverifiable provenance — drop it.
			return "", "", false, nil
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

// gitOutputRunner is the subset of gitcmd.Runner the provenance check needs.
// Declaring it as an interface lets tests inject a failing/slow runner to
// exercise the fail-closed path without a real git repo or a real timeout.
type gitOutputRunner interface {
	Output(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// localConfigGitRunner runs git with a sanitized environment (no inherited
// GIT_* vars, no global/system config) so provenance checks cannot be steered
// by ambient config while still honoring the user's safe.directory trust.
var localConfigGitRunner gitOutputRunner = gitcmd.New()

// gitWorktreePresent reports whether dir sits inside a git worktree, decided
// purely by walking the filesystem upward for a `.git` directory or file
// (the file form covers submodules and linked worktrees). It intentionally
// does not shell out to git, so it still answers correctly when the git
// binary is missing, broken, or slow — exactly the conditions under which the
// provenance query itself cannot determine tracked status.
//
// Both the lexical path and its symlink-resolved physical path are walked: a
// workspace anchored through a symlink (e.g. --workspace /elsewhere/link →
// /repo/nested/ws) has lexical parents with no .git even though the physical
// location is inside a repo, and missing that would let a committed override
// through the "no repo → honor" branch when the git query fails. If the
// physical path cannot be resolved at all, repository membership cannot be
// ruled out, so the check fails closed and reports a worktree.
func gitWorktreePresent(dir string) bool {
	if dotGitInAncestors(dir) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return true
	}
	return resolved != dir && dotGitInAncestors(resolved)
}

func dotGitInAncestors(dir string) bool {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// localConfigTrackState reports whether the .kata.local.toml in root is tracked
// by git, as a tri-state: (tracked, determined). A tracked file is one that was
// committed into the repo (including a force-add past the gitignore), which
// gives its contents attacker-controlled provenance; callers must refuse to
// honor its [server].url / allow_insecure overrides.
//
//   - determined && tracked   → committed; refuse.
//   - determined && !tracked  → untracked/gitignored; honor (the legitimate
//     per-developer remote workflow).
//   - !determined             → git failed or timed out; tracked status is
//     UNKNOWN and the caller must fail closed with an error inside a worktree
//     (see findLocalConfig). Failing open here is attacker-influenceable: a hostile
//     repo can commit an evil override and then induce the git query to fail
//     (a huge index blowing the timeout, a dubious-ownership checkout git
//     refuses) to get the committed URL honored.
//
// The match is case-insensitive and must not depend on git's core.ignorecase:
// on a case-insensitive filesystem (macOS/APFS) findLocalConfig opens a
// committed `.KATA.LOCAL.TOML` via its lowercase stat, so a plain lowercase
// pathspec could miss the uppercase tracked entry and honor the redirect. The
// `:(icase)` pathspec magic performs the case-insensitive match explicitly and
// is anchored to root (no wildcard, so it never matches a same-named file in a
// subdirectory). Scoping the query to the single filename also avoids
// enumerating every tracked file under root, which on a large repo could blow
// the 5s timeout and — because we now fail closed inside a worktree — wrongly
// lock a developer out of a legitimate override.
func localConfigTrackState(root string) (tracked, determined bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := localConfigGitRunner.Output(ctx, root,
		"ls-files", "-z", "--", ":(icase)"+config.LocalConfigFilename)
	if err != nil {
		return false, false
	}
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" {
			continue
		}
		// The :(icase) pathspec matches only the root-level file (a nested
		// same-named file is never returned), but git may print it either
		// cwd-relative (".kata.local.toml") or repo-relative
		// ("workspaces/app/.kata.local.toml") depending on version/config.
		// Compare the basename (git always separates with "/") so a
		// committed override is recognized regardless of the printed form;
		// rejecting slashed entries would miss it and honor the redirect.
		name := entry
		if i := strings.LastIndex(entry, "/"); i >= 0 {
			name = entry[i+1:]
		}
		if strings.EqualFold(name, config.LocalConfigFilename) {
			return true, true
		}
	}
	return false, true
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
	return config.CanonicalHTTPOrigin(u.String())
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

// probeRemote checks an explicitly configured remote using the normal
// non-streaming request budget. Connection refusal still fails immediately,
// while a black-holed or misconfigured remote can wait for the full default or
// operator-selected KATA_HTTP_TIMEOUT budget. Local runtime discovery keeps its
// separate one-second probe so auto-start remains responsive.
//
// Indirected through a package-level var so tests that exercise
// resolution paths past the probe (e.g. allow_insecure bypass) can
// stub the network call without dialing TEST-NET addresses.
var probeRemote = func(ctx context.Context, base string) bool {
	timeout, _ := ParseHTTPTimeout(os.Getenv(HTTPTimeoutEnvVar), DefaultHTTPTimeout)
	client := &http.Client{Timeout: timeout}
	return Ping(ctx, client, base)
}
