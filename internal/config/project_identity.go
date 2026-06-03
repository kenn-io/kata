package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitremote "go.kenn.io/kit/git/remote"
	gitrepo "go.kenn.io/kit/git/repo"
)

// NameChoice is the project name resolved by PickInitName.
type NameChoice struct {
	Name string
}

// ErrNameConflict signals that an existing .kata.toml declares a name
// different from the one the caller supplied. Callers must require an
// explicit replace=true to override.
var ErrNameConflict = errors.New(".kata.toml declares a different project name")

// ErrNoNameSource signals that no project name could be derived: there is no
// .kata.toml, no caller-supplied name, and no workspace root.
var ErrNoNameSource = errors.New("cannot derive project name outside a workspace")

// PickInitName decides the project name for kata init.
func PickInitName(ctx context.Context, disc DiscoveredPaths, tomlCfg *ProjectConfig, inputName string, replace bool) (NameChoice, error) {
	inputName = strings.TrimSpace(inputName)
	switch {
	case tomlCfg != nil && inputName != "" && tomlCfg.Project.Name != "" && tomlCfg.Project.Name != inputName:
		if !replace {
			return NameChoice{}, ErrNameConflict
		}
		return validateNameChoice(inputName)
	case tomlCfg != nil && strings.TrimSpace(tomlCfg.Project.Name) != "":
		return validateNameChoice(strings.TrimSpace(tomlCfg.Project.Name))
	case inputName != "":
		return validateNameChoice(inputName)
	default:
		name, err := DeriveProjectName(ctx, disc)
		if err != nil {
			return NameChoice{}, err
		}
		return validateNameChoice(name)
	}
}

func validateNameChoice(name string) (NameChoice, error) {
	if err := ValidateProjectName(name); err != nil {
		return NameChoice{}, err
	}
	return NameChoice{Name: name}, nil
}

// PickName returns explicit if non-empty, otherwise the last `/` or
// `:`-separated segment of identity (so "github.com/wesm/kata" → "kata").
func PickName(explicit, identity string) string {
	if explicit != "" {
		return explicit
	}
	return lastSegment(identity)
}

// DeriveProjectName derives the default project name for init from the
// discovered workspace. Git remotes use the last normalized remote segment;
// local workspaces use the directory name.
func DeriveProjectName(ctx context.Context, d DiscoveredPaths) (string, error) {
	if d.GitRoot != "" {
		remote, err := readGitRemote(ctx, d.GitRoot)
		if err != nil {
			return "", err
		}
		if remote != "" {
			id, err := NormalizeRemoteURL(remote)
			if err != nil {
				return "", err
			}
			return lastSegment(id), nil
		}
		return filepath.Base(d.GitRoot), nil
	}
	if d.WorkspaceRoot != "" {
		return filepath.Base(d.WorkspaceRoot), nil
	}
	return "", ErrNoNameSource
}

// WriteDestination returns the directory where .kata.toml should be
// written for a kata init invocation: workspace root if discovered,
// else git root, else the absolute start path.
func WriteDestination(disc DiscoveredPaths, startPath string) string {
	if disc.WorkspaceRoot != "" {
		return disc.WorkspaceRoot
	}
	if disc.GitRoot != "" {
		return disc.GitRoot
	}
	return startPath
}

// DiscoveredPaths is the result of walking upward from a start path.
// Both fields may be empty (no .kata.toml and no .git ancestor).
type DiscoveredPaths struct {
	WorkspaceRoot string // first ancestor with .kata.toml (inclusive)
	GitRoot       string // first ancestor with .git (inclusive)
}

// AliasInfo is the alias-identity record derived from a workspace.
type AliasInfo struct {
	Identity string // git remote (normalized) or "local://<abs path>"
	Kind     string // "git" | "local"
}

// DiscoverPaths walks upward from startPath looking for .kata.toml (W) and
// .git (G). Both lookups are independent and inclusive of startPath itself.
// startPath must point at an existing file or directory; a missing start path
// is reported as a resolution error so a typo like `--workspace /no/such/dir`
// fails loud instead of silently resolving an unrelated ancestor.
// Errors from os.Stat that aren't "not exist" (permission denied, etc.) on
// ancestor lookups also propagate.
func DiscoverPaths(startPath string) (DiscoveredPaths, error) {
	abs, err := filepath.Abs(startPath)
	if err != nil {
		return DiscoveredPaths{}, fmt.Errorf("abs %s: %w", startPath, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return DiscoveredPaths{}, fmt.Errorf("stat %s: %w", abs, err)
	}
	// If the start path is a file, walk from its parent directory so we don't
	// stat <file>/.kata.toml (which fails with ENOTDIR rather than not-found).
	walkRoot := abs
	if !info.IsDir() {
		walkRoot = filepath.Dir(abs)
	}
	d := DiscoveredPaths{}
	if d.WorkspaceRoot, err = walkUp(walkRoot, ProjectConfigFilename, false); err != nil {
		return DiscoveredPaths{}, fmt.Errorf("discover %s: %w", ProjectConfigFilename, err)
	}
	if d.GitRoot, err = walkUp(walkRoot, ".git", true); err != nil {
		return DiscoveredPaths{}, fmt.Errorf("discover .git: %w", err)
	}
	return d, nil
}

// walkUp returns the first ancestor (inclusive) containing the named entry,
// or "" if none. allowDir lets the entry be either a file or directory.
// os.IsNotExist failures are normal during traversal; other errors surface so
// callers see e.g. permission-denied instead of treating it as "not found".
func walkUp(start, entry string, allowDir bool) (string, error) {
	dir := start
	for {
		path := filepath.Join(dir, entry)
		info, err := os.Stat(path)
		switch {
		case err == nil:
			if info.IsDir() {
				if allowDir {
					return dir, nil
				}
			} else {
				return dir, nil
			}
		case !os.IsNotExist(err):
			return "", fmt.Errorf("stat %s: %w", path, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// ComputeAliasIdentity derives the alias for a workspace per spec §2.4. Order:
// 1. GitRoot with remote → normalized origin URL
// 2. GitRoot without remote → local://<abs(GitRoot)>
// 3. WorkspaceRoot only → local://<abs(WorkspaceRoot)>
// 4. Neither → error
func ComputeAliasIdentity(ctx context.Context, d DiscoveredPaths) (AliasInfo, error) {
	if d.GitRoot != "" {
		remote, err := readGitRemote(ctx, d.GitRoot)
		if err != nil {
			return AliasInfo{}, err
		}
		if remote != "" {
			id, err := NormalizeRemoteURL(remote)
			if err != nil {
				return AliasInfo{}, err
			}
			return AliasInfo{Identity: id, Kind: "git"}, nil
		}
		return AliasInfo{
			Identity: "local://" + d.GitRoot,
			Kind:     "local",
		}, nil
	}
	if d.WorkspaceRoot != "" {
		return AliasInfo{
			Identity: "local://" + d.WorkspaceRoot,
			Kind:     "local",
		}, nil
	}
	return AliasInfo{}, fmt.Errorf("no workspace or git root discovered")
}

// readGitRemote returns the URL of "origin" (or the first remote listed by
// `git remote` when no origin exists). Returns ("", nil) if no remotes.
func readGitRemote(ctx context.Context, gitRoot string) (string, error) {
	remotes, err := gitrepo.ListRemotes(ctx, gitRoot)
	if err != nil {
		return "", fmt.Errorf("git remote: %w", err)
	}
	if len(remotes) == 0 {
		return "", nil
	}
	target := "origin"
	hasOrigin := false
	for _, r := range remotes {
		if r == "origin" {
			hasOrigin = true
			break
		}
	}
	if !hasOrigin {
		target = remotes[0]
	}
	out, err := gitRunner.Output(ctx, gitRoot, "remote", "get-url", target)
	if err != nil {
		return "", fmt.Errorf("git remote get-url %s: %w", target, err)
	}
	return strings.TrimSpace(string(out)), nil
}

var gitRunner = gitcmd.New()

// NormalizeRemoteURL strips credentials, normalizes SSH↔HTTPS, drops trailing
// .git, and returns "host/path" form (e.g. "github.com/wesm/kata").
// Percent-encoded characters are decoded and spaces replaced with hyphens so
// the result satisfies the identity charset (Azure DevOps remotes commonly
// contain %20 in org/project names).
func NormalizeRemoteURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty remote url")
	}
	host := gitremote.RemoteHost(raw)
	if host == "" {
		return "", fmt.Errorf("parse remote url %q: not a recognized form", raw)
	}
	result := host
	if path := gitremote.RemoteRepoPath(raw); path != "" {
		result += "/" + path
	}
	if decoded, err := url.PathUnescape(result); err == nil {
		result = decoded
	}
	result = strings.ReplaceAll(result, " ", "-")
	return result, nil
}

var identityCharsetRE = regexp.MustCompile(`^[A-Za-z0-9._:/\-]+$`)

// ValidateIdentity enforces the spec §2.4 charset and forbids whitespace and
// embedded URL credentials.
func ValidateIdentity(id string) error {
	if id == "" {
		return fmt.Errorf("identity must be non-empty")
	}
	for _, r := range id {
		if unicode.IsSpace(r) {
			return fmt.Errorf("identity contains whitespace: %q", id)
		}
	}
	if strings.HasPrefix(id, "http://") || strings.HasPrefix(id, "https://") {
		// reject embedded credentials.
		if strings.Contains(id, "@") {
			return fmt.Errorf("identity must not embed credentials: %q", id)
		}
	}
	if !identityCharsetRE.MatchString(stripLocalScheme(id)) {
		return fmt.Errorf("identity contains disallowed characters: %q", id)
	}
	return nil
}

// stripLocalScheme allows local://<abs path> identities through the charset
// check by ignoring the scheme prefix and validating the remainder.
func stripLocalScheme(id string) string {
	const prefix = "local://"
	if strings.HasPrefix(id, prefix) {
		return strings.ReplaceAll(id[len(prefix):], "/", "")
	}
	return id
}

// ValidateAliasInfo enforces wire-level rules on alias metadata
// supplied by remote clients. Unlike ValidateIdentity (which gates a
// project's canonical identity on a strict charset), aliases of kind
// "local" carry workspace paths and must be allowed to contain
// spaces and other characters real filesystems use.
//
//   - kind: "git" or "local".
//   - identity (kind=git): apply ValidateIdentity, since a git alias
//     is a normalized remote URL and obeys the same rules as project
//     identity.
//   - identity (kind=local): must start with "local://" and have a
//     non-empty path component. No charset check — the path can
//     contain anything the filesystem accepts.
func ValidateAliasInfo(info AliasInfo) error {
	if info.Kind != "git" && info.Kind != "local" {
		return fmt.Errorf("alias.kind must be \"git\" or \"local\", got %q", info.Kind)
	}
	if info.Identity == "" {
		return fmt.Errorf("alias.identity must be non-empty")
	}
	switch info.Kind {
	case "git":
		if err := ValidateIdentity(info.Identity); err != nil {
			return fmt.Errorf("alias.identity: %w", err)
		}
	case "local":
		const prefix = "local://"
		if !strings.HasPrefix(info.Identity, prefix) || info.Identity == prefix {
			return fmt.Errorf("alias.identity for kind=local must be %s<path>", prefix)
		}
	}
	return nil
}
