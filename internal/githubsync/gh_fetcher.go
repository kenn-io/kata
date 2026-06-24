package githubsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Fetcher reads GitHub repository data needed by the sync importer.
type Fetcher interface {
	Repository(ctx context.Context, host, owner, repo string) (Repository, error)
	Issues(ctx context.Context, binding Binding, since *time.Time) ([]Issue, error)
	Comments(ctx context.Context, binding Binding, issueNumber int) ([]Comment, error)
}

// CommandRunner executes a command and returns stdout, stderr, and the command error.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error)
}

// GHFetcher reads GitHub data through gh api.
type GHFetcher struct {
	runner CommandRunner
}

// NewGHFetcher returns a GitHub fetcher backed by runner.
func NewGHFetcher(runner CommandRunner) *GHFetcher {
	if runner == nil {
		runner = execCommandRunner{}
	}
	return &GHFetcher{runner: runner}
}

// Repository validates and reads a GitHub repository.
func (f *GHFetcher) Repository(ctx context.Context, host, owner, repo string) (Repository, error) {
	binding, err := normalizeBinding(Binding{Host: host, Owner: owner, Repo: repo})
	if err != nil {
		return Repository{}, err
	}
	stdout, stderr, err := f.runner.Run(ctx, "gh", "api", "--hostname", binding.Host, repositoryEndpoint(binding))
	if err != nil {
		return Repository{}, ghAPIError(stderr, err)
	}
	var out Repository
	if err := json.Unmarshal(stdout, &out); err != nil {
		return Repository{}, fmt.Errorf("decode GitHub repository: %w", err)
	}
	return out, nil
}

// Issues reads repository issues using gh api pagination slurp output.
func (f *GHFetcher) Issues(ctx context.Context, binding Binding, since *time.Time) ([]Issue, error) {
	binding, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := f.runner.Run(ctx, "gh", "api", "--hostname", binding.Host, "--paginate", "--slurp", issuesEndpoint(binding, since))
	if err != nil {
		return nil, ghAPIError(stderr, err)
	}
	issues, err := DecodeSlurpArray[Issue](stdout)
	if err != nil {
		return nil, err
	}
	return issues, nil
}

// Comments reads issue comments for one issue number using gh api pagination slurp output.
func (f *GHFetcher) Comments(ctx context.Context, binding Binding, issueNumber int) ([]Comment, error) {
	if issueNumber <= 0 {
		return nil, fmt.Errorf("GitHub issue number must be positive")
	}
	binding, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := f.runner.Run(ctx, "gh", "api", "--hostname", binding.Host, "--paginate", "--slurp", commentsEndpoint(binding, issueNumber))
	if err != nil {
		return nil, ghAPIError(stderr, err)
	}
	comments, err := DecodeSlurpArray[Comment](stdout)
	if err != nil {
		return nil, err
	}
	return comments, nil
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

func ghAPIError(stderr []byte, err error) error {
	errText := redactTokenMaterial(err.Error())
	stderrText := strings.TrimSpace(redactTokenMaterial(string(stderr)))
	if stderrText == "" {
		return fmt.Errorf("gh api failed: %s", errText)
	}
	return fmt.Errorf("gh api failed: %s: %s", errText, stderrText)
}

var tokenRedactions = []*regexp.Regexp{
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
