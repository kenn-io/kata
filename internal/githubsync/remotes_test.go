package githubsync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGitHubRemote(t *testing.T) {
	tests := []struct {
		name         string
		remote       string
		allowedHosts string
		host         string
		owner        string
		repo         string
	}{
		{
			name:   "https github dot com",
			remote: "https://github.com/example-org/example-repo.git",
			host:   "github.com",
			owner:  "example-org",
			repo:   "example-repo",
		},
		{
			name:   "scp style github dot com",
			remote: "git@github.com:example-org/example-repo.git",
			host:   "github.com",
			owner:  "example-org",
			repo:   "example-repo",
		},
		{
			name:         "ssh github enterprise",
			remote:       "ssh://git@github.example/example-org/example-repo.git",
			allowedHosts: "github.example",
			host:         "github.example",
			owner:        "example-org",
			repo:         "example-repo",
		},
		{
			name:   "strips only one git suffix",
			remote: "https://github.com/example-org/example-repo.git.git",
			host:   "github.com",
			owner:  "example-org",
			repo:   "example-repo.git",
		},
		{
			name:   "normalized github dot com alias",
			remote: "github.com/example-org/example-repo",
			host:   "github.com",
			owner:  "example-org",
			repo:   "example-repo",
		},
		{
			name:   "normalized github dot com alias with git suffix",
			remote: "github.com/example-org/example-repo.git",
			host:   "github.com",
			owner:  "example-org",
			repo:   "example-repo",
		},
		{
			name:         "normalized github enterprise alias",
			remote:       "github.example/example-org/example-repo",
			allowedHosts: "github.example",
			host:         "github.example",
			owner:        "example-org",
			repo:         "example-repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.allowedHosts != "" {
				t.Setenv("KATA_GITHUB_SYNC_ALLOWED_HOSTS", tt.allowedHosts)
			}
			got, err := ParseGitHubRemote(tt.remote)
			require.NoError(t, err)
			assert.Equal(t, tt.host, got.Host)
			assert.Equal(t, tt.owner, got.Owner)
			assert.Equal(t, tt.repo, got.Repo)
		})
	}
}

func TestParseGitHubRemoteRejectsUntrustedGitHubEnterpriseHosts(t *testing.T) {
	tests := []string{
		"https://github.example/example-org/example-repo.git",
		"https://github.attacker.example/example-org/example-repo.git",
		"git@github.attacker.example:example-org/example-repo.git",
	}

	for _, remote := range tests {
		t.Run(remote, func(t *testing.T) {
			_, err := ParseGitHubRemote(remote)
			require.Error(t, err)
		})
	}
}

func TestParseGitHubRemoteRejectsInvalidIdentities(t *testing.T) {
	tests := []string{
		"",
		"not-a-remote",
		"https://gitlab.com/example-org/example-repo.git",
		"https://github.com/example-org",
		"https://github.com/example-org/example-repo/extra",
		"git@github.com:example-org",
		"git@github.com:/example-repo.git",
		"ssh://git@example.com/example-org/example-repo.git",
		"github.com/example-org",
		"github.com/example-org/example-repo/extra",
		"github.com//example-repo",
		"github.com/example-org/",
	}

	for _, remote := range tests {
		t.Run(remote, func(t *testing.T) {
			_, err := ParseGitHubRemote(remote)
			require.Error(t, err)
		})
	}
}

func TestParseGitHubRemoteRedactsCredentialedRemoteErrors(t *testing.T) {
	tests := []string{
		"https://ghp_exampleSecretMaterial@github.com/example-org",
		"https://ghp_exampleSecretMaterial@github.com/%zz/example-repo",
	}

	for _, remote := range tests {
		t.Run(remote, func(t *testing.T) {
			_, err := ParseGitHubRemote(remote)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "ghp_exampleSecretMaterial")
			assert.NotContains(t, err.Error(), remote)
		})
	}
}
