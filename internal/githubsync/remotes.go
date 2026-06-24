package githubsync

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

const githubSyncAllowedHostsEnv = "KATA_GITHUB_SYNC_ALLOWED_HOSTS"

// ParseGitHubRemote extracts a GitHub host, owner, and repository from a git remote URL.
func ParseGitHubRemote(remote string) (Binding, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return Binding{}, fmt.Errorf("parse GitHub remote: empty remote")
	}
	if strings.HasPrefix(remote, "git@") {
		return parseSCPStyleGitHubRemote(remote)
	}
	if !strings.Contains(remote, "://") && strings.Contains(remote, "/") {
		return parseBareGitHubRemote(remote)
	}
	return parseURLGitHubRemote(remote)
}

func parseSCPStyleGitHubRemote(remote string) (Binding, error) {
	hostAndPath := strings.TrimPrefix(remote, "git@")
	host, path, ok := strings.Cut(hostAndPath, ":")
	if !ok {
		return Binding{}, fmt.Errorf("parse GitHub remote: missing repository path")
	}
	return bindingFromRemoteParts(host, strings.Split(path, "/"))
}

func parseBareGitHubRemote(remote string) (Binding, error) {
	parts := strings.Split(remote, "/")
	if len(parts) < 1 {
		return Binding{}, fmt.Errorf("parse GitHub remote: expected host/owner/repo path")
	}
	return bindingFromRemoteParts(parts[0], parts[1:])
}

func parseURLGitHubRemote(remote string) (Binding, error) {
	u, err := url.Parse(remote)
	if err != nil {
		return Binding{}, fmt.Errorf("parse GitHub remote: invalid URL")
	}
	switch u.Scheme {
	case "https", "ssh":
	default:
		return Binding{}, fmt.Errorf("parse GitHub remote: unsupported scheme %q", u.Scheme)
	}
	if u.Scheme == "ssh" && u.User != nil && u.User.Username() != "git" {
		return Binding{}, fmt.Errorf("parse GitHub remote: unsupported ssh user")
	}
	if u.Host == "" {
		return Binding{}, fmt.Errorf("parse GitHub remote: missing host")
	}
	path := strings.TrimPrefix(u.EscapedPath(), "/")
	parts := strings.Split(path, "/")
	for i, part := range parts {
		unescaped, err := url.PathUnescape(part)
		if err != nil {
			return Binding{}, fmt.Errorf("parse GitHub remote: invalid path escaping: %w", err)
		}
		parts[i] = unescaped
	}
	return bindingFromRemoteParts(u.Hostname(), parts)
}

func bindingFromRemoteParts(host string, parts []string) (Binding, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if !isGitHubHost(host) {
		return Binding{}, fmt.Errorf("parse GitHub remote: unsupported host %q", host)
	}
	if len(parts) != 2 {
		return Binding{}, fmt.Errorf("parse GitHub remote: expected owner/repo path")
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return Binding{}, fmt.Errorf("parse GitHub remote: owner and repo are required")
	}
	repo = strings.TrimSuffix(repo, ".git")
	if repo == "" || strings.Contains(owner, "/") || strings.Contains(repo, "/") {
		return Binding{}, fmt.Errorf("parse GitHub remote: malformed owner or repo")
	}
	return Binding{Host: host, Owner: owner, Repo: repo}, nil
}

func isGitHubHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "github.com" {
		return true
	}
	for _, allowed := range strings.Split(os.Getenv(githubSyncAllowedHostsEnv), ",") {
		if host == strings.ToLower(strings.TrimSpace(allowed)) {
			return host != ""
		}
	}
	return false
}
