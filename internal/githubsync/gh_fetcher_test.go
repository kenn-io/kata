package githubsync

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGHFetcherRepository(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{stdout: []byte(`{"node_id":"R_example","id":123456789012,"full_name":"example-org/example-repo"}`)},
		},
	}
	fetcher := NewGHFetcher(runner)

	repo, err := fetcher.Repository(context.Background(), "github.example", "example-org", "example-repo")
	require.NoError(t, err)

	assert.Equal(t, "R_example", repo.NodeID)
	assert.Equal(t, int64(123456789012), repo.ID)
	assert.Equal(t, "example-org/example-repo", repo.FullName)
	require.Len(t, runner.calls, 1)
	assert.Equal(t, "gh", runner.calls[0].name)
	assert.Equal(t, []string{"api", "--hostname", "github.example", "repos/example-org/example-repo"}, runner.calls[0].args)
}

func TestGHFetcherIssues(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{stdout: []byte(`[
				[{"node_id":"I_first","number":1,"title":"first"}],
				[{"node_id":"I_second","number":2,"title":"second"}]
			]`)},
		},
	}
	fetcher := NewGHFetcher(runner)
	binding := Binding{Host: "github.example", Owner: "example-org", Repo: "example-repo"}

	issues, err := fetcher.Issues(context.Background(), binding, nil)
	require.NoError(t, err)

	require.Len(t, issues, 2)
	assert.Equal(t, "I_first", issues[0].NodeID)
	assert.Equal(t, "I_second", issues[1].NodeID)
	require.Len(t, runner.calls, 1)
	assert.Equal(t, "gh", runner.calls[0].name)
	require.Len(t, runner.calls[0].args, 6)
	assert.Equal(t, []string{"api", "--hostname", "github.example", "--paginate", "--slurp"}, runner.calls[0].args[:5])
	path, query := parseAPIEndpoint(t, runner.calls[0].args[5])
	assert.Equal(t, "repos/example-org/example-repo/issues", path)
	assert.Equal(t, "all", query.Get("state"))
	assert.Equal(t, "updated", query.Get("sort"))
	assert.Equal(t, "asc", query.Get("direction"))
	assert.Equal(t, "100", query.Get("per_page"))
	assert.Empty(t, query.Get("since"))
}

func TestGHFetcherIssuesIncludesSince(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{stdout: []byte(`[[]]`)},
		},
	}
	fetcher := NewGHFetcher(runner)
	binding := Binding{Host: "github.example", Owner: "example-org", Repo: "example-repo"}
	since := time.Date(2026, 6, 22, 10, 30, 15, 0, time.UTC)

	_, err := fetcher.Issues(context.Background(), binding, &since)
	require.NoError(t, err)

	require.Len(t, runner.calls, 1)
	path, query := parseAPIEndpoint(t, runner.calls[0].args[5])
	assert.Equal(t, "repos/example-org/example-repo/issues", path)
	assert.Equal(t, since.Format(time.RFC3339), query.Get("since"))
}

func TestGHFetcherComments(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{stdout: []byte(`[
				[{"node_id":"C_first","body":"first"}],
				[{"node_id":"C_second","body":"second"}]
			]`)},
		},
	}
	fetcher := NewGHFetcher(runner)
	binding := Binding{Host: "github.example", Owner: "example-org", Repo: "example-repo"}

	comments, err := fetcher.Comments(context.Background(), binding, 42)
	require.NoError(t, err)

	require.Len(t, comments, 2)
	assert.Equal(t, "C_first", comments[0].NodeID)
	assert.Equal(t, "C_second", comments[1].NodeID)
	require.Len(t, runner.calls, 1)
	assert.Equal(t, "gh", runner.calls[0].name)
	require.Len(t, runner.calls[0].args, 6)
	assert.Equal(t, []string{"api", "--hostname", "github.example", "--paginate", "--slurp"}, runner.calls[0].args[:5])
	path, query := parseAPIEndpoint(t, runner.calls[0].args[5])
	assert.Equal(t, "repos/example-org/example-repo/issues/42/comments", path)
	assert.Equal(t, "100", query.Get("per_page"))
}

func TestGHFetcherRejectsUntrustedGitHubEnterpriseHost(t *testing.T) {
	runner := &fakeCommandRunner{}
	fetcher := NewGHFetcher(runner)

	_, err := fetcher.Repository(context.Background(), "github.attacker.example", "example-org", "example-repo")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
	assert.Empty(t, runner.calls)
}

func TestGHFetcherRedactsTokenMaterialFromCommandErrors(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	token := "ghp_" + "exampleSecretMaterial"
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{
				stderr: []byte("request failed with token " + token),
				err:    errors.New("exit status 1"),
			},
		},
	}
	fetcher := NewGHFetcher(runner)

	_, err := fetcher.Repository(context.Background(), "github.example", "example-org", "example-repo")
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, "gh api")
	assert.Contains(t, message, "[redacted]")
	assert.NotContains(t, message, token)
	require.Len(t, runner.calls, 1)
	assert.NotContains(t, strings.Join(runner.calls[0].args, " "), "auth token")
}

func allowGitHubEnterpriseHost(t *testing.T, host string) {
	t.Helper()
	t.Setenv("KATA_GITHUB_SYNC_ALLOWED_HOSTS", host)
}

type fakeCommandRunner struct {
	calls     []fakeCommandCall
	responses []fakeCommandResponse
}

func (r *fakeCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, fakeCommandCall{name: name, args: append([]string(nil), args...)})
	if len(r.responses) == 0 {
		return nil, nil, errors.New("unexpected command")
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response.stdout, response.stderr, response.err
}

type fakeCommandCall struct {
	name string
	args []string
}

type fakeCommandResponse struct {
	stdout []byte
	stderr []byte
	err    error
}

func parseAPIEndpoint(t *testing.T, endpoint string) (string, url.Values) {
	t.Helper()
	u, err := url.Parse("https://daemon.example/" + endpoint)
	require.NoError(t, err)
	return strings.TrimPrefix(u.Path, "/"), u.Query()
}
