package githubsync

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParentData carries GitHub parent relationship data plus scan coverage.
type ParentData struct {
	ParentByChild   map[int]int64
	ScannedChildren map[int]struct{}
	ChildIDByNumber map[int]int64
	Authoritative   bool
	Unsupported     bool
}

// ParentID returns the parent REST ID for childNumber when GitHub reported one.
func (d ParentData) ParentID(childNumber int) (int64, bool) {
	if d.ParentByChild == nil {
		return 0, false
	}
	parentID, ok := d.ParentByChild[childNumber]
	if !ok || parentID == 0 {
		return 0, false
	}
	return parentID, true
}

// ChildScanned reports whether the parent scan included childNumber.
func (d ParentData) ChildScanned(childNumber int) bool {
	if d.ScannedChildren == nil {
		return false
	}
	_, ok := d.ScannedChildren[childNumber]
	return ok
}

// Fetcher reads GitHub repository data needed by the sync importer.
type Fetcher interface {
	Repository(ctx context.Context, host, owner, repo string) (Repository, error)
	Issues(ctx context.Context, binding Binding, since *time.Time) ([]Issue, error)
	Comments(ctx context.Context, binding Binding, issueNumber int) ([]Comment, error)
	// ParentData returns child issue parent REST IDs plus the child numbers
	// covered by the scan. Unsupported providers should return Unsupported.
	ParentData(ctx context.Context, binding Binding) (ParentData, error)
}

// BindingSessionFetcher can provide a Fetcher that reuses binding-scoped
// resources such as authenticated transports for one sync run.
type BindingSessionFetcher interface {
	ForBinding(ctx context.Context, binding Binding) (Fetcher, error)
}

// CommandRunner executes a command and returns stdout, stderr, and the command error.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	if name != "gh" {
		return nil, nil, fmt.Errorf("unsupported command %q", name)
	}
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- name is fixed to gh and path comes from exec.LookPath.
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func normalizeBinding(binding Binding) (Binding, error) {
	host := strings.ToLower(strings.TrimSpace(binding.Host))
	owner := strings.TrimSpace(binding.Owner)
	repo := strings.TrimSpace(binding.Repo)
	if !isGitHubHost(host) {
		return Binding{}, fmt.Errorf("GitHub host %q is not supported", binding.Host)
	}
	if owner == "" || repo == "" {
		return Binding{}, fmt.Errorf("GitHub owner and repo are required")
	}
	if strings.Contains(owner, "/") || strings.Contains(repo, "/") {
		return Binding{}, fmt.Errorf("GitHub owner and repo must be path segments")
	}
	return Binding{Host: host, Owner: owner, Repo: repo}, nil
}

func repositoryEndpoint(binding Binding) string {
	return "repos/" + url.PathEscape(binding.Owner) + "/" + url.PathEscape(binding.Repo)
}

func issuesEndpoint(binding Binding, since *time.Time) string {
	query := url.Values{}
	query.Set("direction", "asc")
	query.Set("per_page", "100")
	query.Set("sort", "updated")
	query.Set("state", "all")
	if since != nil {
		query.Set("since", since.Format(time.RFC3339))
	}
	return repositoryEndpoint(binding) + "/issues?" + query.Encode()
}

func commentsEndpoint(binding Binding, issueNumber int) string {
	query := url.Values{}
	query.Set("per_page", "100")
	return repositoryEndpoint(binding) + "/issues/" + strconv.Itoa(issueNumber) + "/comments?" + query.Encode()
}

var tokenRedactions = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]+`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]+`),
	regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|token)\s+)\S+`),
	regexp.MustCompile(`(?i)((?:bearer|token)\s+)\S+`),
}

func redactTokenMaterial(s string) string {
	for _, pattern := range tokenRedactions {
		s = pattern.ReplaceAllString(s, "${1}[redacted]")
	}
	return s
}
